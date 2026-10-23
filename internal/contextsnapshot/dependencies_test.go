package contextsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"themisy/adapters/datadog"
	"themisy/internal/domain"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

type dependencySource struct {
	calls  int
	failAt int
}

func (s *dependencySource) Credential(context.Context) (credentials.Credential, error) {
	s.calls++
	if s.calls == s.failAt {
		return credentials.Credential{}, errors.New("secret-canary")
	}
	return credentials.New(map[string][]byte{"api_key": []byte("secret-canary"), "app_key": []byte("secret-canary")}, time.Now().Add(time.Hour)), nil
}

type dependencyHTTP struct {
	calls int
	mode  string
}

func (h *dependencyHTTP) Do(r *http.Request) (*http.Response, error) {
	h.calls++
	if r.Method != "GET" || r.URL.Host != "api.datadoghq.eu" {
		return nil, errors.New("unexpected request")
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	value := 0.2
	if h.mode == "fail" {
		value = 10
	}
	status := 200
	if h.mode == "rate" {
		status = 429
	}
	series := []any{map[string]any{"pointlist": [][2]any{{(to - 30) * 1000, value}, {to * 1000, value}}}}
	if h.mode == "missing" {
		series = nil
	}
	if h.mode == "partial" && h.calls == 2 {
		status = 503
	}
	body, _ := json.Marshal(map[string]any{"status": "ok", "query": r.URL.Query().Get("query"), "from_date": from * 1000, "to_date": to * 1000, "series": series})
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}
func dependencyFixture(t *testing.T) (*DependencyCollector, *dependencySource, *dependencyHTTP, *ciAuditSink, DependencyConfig, DependencyKey) {
	t.Helper()
	key := DependencyKey{"tenant-a", "checkout", "staging"}
	target := datadog.HealthTarget{Site: datadog.SiteEU1, Window: 5 * time.Minute, Query: datadog.Query{Expression: "max:private-query{env:staging}", Comparator: "lte", Threshold: 1, Aggregation: "max", MinimumPoints: 2, MaximumAge: 2 * time.Minute}}
	config := DependencyConfig{Sets: map[DependencyKey][]string{key: {"ledger", "catalog"}}, Health: datadog.HealthConfig{Targets: map[datadog.HealthTargetKey]datadog.HealthTarget{{TenantID: "tenant-a", Service: "ledger", Environment: "staging"}: target, {TenantID: "tenant-a", Service: "catalog", Environment: "staging"}: target}}}
	source := &dependencySource{}
	http := &dependencyHTTP{}
	sink := &ciAuditSink{}
	c, e := NewDependencyCollector(config, source, http, sink)
	if e != nil {
		t.Fatal(e)
	}
	return c, source, http, sink, config, key
}
func TestDependenciesCollectWholeTrustedSetAndAudit(t *testing.T) {
	c, source, http, sink, config, key := dependencyFixture(t)
	config.Sets[key][0] = "attacker"
	delete(config.Health.Targets, datadog.HealthTargetKey{TenantID: "tenant-a", Service: "catalog", Environment: "staging"})
	s, e := c.Collect(context.Background(), key)
	if e != nil || s.RequireFreshDependencies(time.Now()) != nil {
		t.Fatalf("%+v %v", s.Record(), e)
	}
	r := s.Record()
	if source.calls != 2 || http.calls != 2 || len(sink.events) != 1 || r.Evidence[0].Target.Service != "catalog" || r.Evidence[1].Target.Service != "ledger" {
		t.Fatal("trusted set not preserved")
	}
	h, e := dependencyHash(r)
	if e != nil || h != s.Hash() || sink.events[0].ResourceID != h {
		t.Fatal("hash/audit mismatch")
	}
	raw, _ := json.Marshal(sink.events[0])
	if strings.Contains(string(raw), "secret-canary") || strings.Contains(string(raw), "private-query") {
		t.Fatal("secret in audit")
	}
	r.Evidence[0].Status = adapter.VerificationFail
	if s.RequireFreshDependencies(time.Now()) != nil {
		t.Fatal("mutable record changed snapshot")
	}
	auditRecord := sink.events[0].Details["record"].(DependencyRecord)
	auditRecord.Evidence[0].Status = adapter.VerificationMissing
	if s.RequireFreshDependencies(time.Now()) != nil {
		t.Fatal("sink changed snapshot")
	}
	var restored DependencySnapshot
	if json.Unmarshal(raw, &restored) == nil || restored.RequireFreshDependencies(time.Now()) == nil {
		t.Fatal("JSON minted trust")
	}
	if s.RequireFreshDependencies(time.Now().Add(3*time.Minute)) == nil || (DependencySnapshot{}).RequireFreshDependencies(time.Now()) == nil {
		t.Fatal("stale/zero accepted")
	}
}
func TestDependenciesPreserveNonPassAndPartialObservations(t *testing.T) {
	for _, mode := range []string{"fail", "missing", "rate", "partial"} {
		t.Run(mode, func(t *testing.T) {
			c, _, http, sink, _, key := dependencyFixture(t)
			http.mode = mode
			s, e := c.Collect(context.Background(), key)
			if e != nil || len(s.Record().Evidence) != 2 || s.RequireFreshDependencies(time.Now()) == nil || len(sink.events) != 1 || sink.events[0].Result == "PASS" {
				t.Fatal("nonpass promoted", e)
			}
			if mode == "partial" && s.Record().Evidence[0].Status != adapter.VerificationPass {
				t.Fatal("lost per-dependency status")
			}
		})
	}
}
func TestDependenciesRejectUnknownPartialAcquisitionAndAuditFailure(t *testing.T) {
	for _, mode := range []string{"tenant", "service", "environment", "credential", "audit", "cancel", "append-cancel"} {
		t.Run(mode, func(t *testing.T) {
			c, source, http, sink, _, key := dependencyFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "tenant":
				key.TenantID = "other"
			case "service":
				key.Service = "other"
			case "environment":
				key.Environment = "other"
			case "credential":
				source.failAt = 2
			case "audit":
				sink.err = errors.New("response lost secret-canary")
			case "cancel":
				cancel()
			}
			if mode == "append-cancel" {
				sink.hook = func(context.Context, domain.AuditEvent) { cancel() }
			}
			s, e := c.Collect(ctx, key)
			if e == nil || s.Hash() != "" {
				t.Fatal("failure returned snapshot")
			}
			if (mode == "tenant" || mode == "service" || mode == "environment" || mode == "cancel") && (source.calls != 0 || http.calls != 0) {
				t.Fatal("unknown/cancelled fetched source")
			}
			if mode == "credential" && len(sink.events) != 0 {
				t.Fatal("partial acquisition audited as complete")
			}
		})
	}
}
func TestDependencyConfigurationRejectsMissingAndDuplicateSets(t *testing.T) {
	for _, mode := range []string{"empty", "duplicate", "unmapped"} {
		t.Run(mode, func(t *testing.T) {
			_, source, http, sink, config, key := dependencyFixture(t)
			switch mode {
			case "empty":
				config.Sets[key] = nil
			case "duplicate":
				config.Sets[key] = []string{"ledger", "ledger"}
			case "unmapped":
				config.Sets[key] = []string{"absent"}
			}
			if _, e := NewDependencyCollector(config, source, http, sink); e == nil {
				t.Fatal("invalid set accepted")
			}
		})
	}
}

func TestDependenciesExpiryBoundaryAndCaptureDelay(t *testing.T) {
	c, _, _, _, _, key := dependencyFixture(t)
	s, err := c.Collect(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	expiry := s.record.Evidence[0].SourceUpdatedAt.Add(s.record.Evidence[0].MaximumAge)
	if s.RequireFreshDependencies(expiry) == nil || s.RequireFreshDependencies(expiry.Add(-time.Nanosecond)) != nil {
		t.Fatal("expiry boundary")
	}
	a := s.Record()
	b := s.Record()
	h1, _ := dependencyHash(a)
	h2, _ := dependencyHash(b)
	if h1 != h2 {
		t.Fatal("same record hashes differently")
	}
	b.Evidence[0].ReasonCode = "changed"
	h2, _ = dependencyHash(b)
	if h1 == h2 {
		t.Fatal("evidence not hash-bound")
	}
	calls := 0
	c.now = func() time.Time {
		calls++
		if calls > 3 {
			return time.Now().Add(3 * time.Minute)
		}
		return time.Now()
	}
	delayed, err := c.Collect(context.Background(), key)
	if err != nil || delayed.RequireFreshDependencies(delayed.Record().CapturedAt) == nil {
		t.Fatal("slow collection promoted stale evidence", err)
	}
}
