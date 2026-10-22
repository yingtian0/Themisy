package datadog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

type healthTransport struct {
	body    string
	status  int
	err     error
	calls   int
	request *http.Request
	hook    func()
}

func (h *healthTransport) Do(r *http.Request) (*http.Response, error) {
	h.calls++
	h.request = r.Clone(r.Context())
	if h.hook != nil {
		h.hook()
	}
	if h.err != nil {
		return nil, h.err
	}
	return &http.Response{StatusCode: h.status, Body: io.NopCloser(strings.NewReader(h.body)), Request: r}, nil
}

func healthFixture(t *testing.T) (*HealthReader, *healthTransport, HealthConfig, HealthRequest, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 22, 0, 5, 0, 0, time.UTC)
	target := HealthTarget{Site: SiteEU1, Query: Query{Expression: "max:dependency.error_rate{service:ledger,env:staging}", Comparator: "lte", Threshold: 1, Aggregation: "max", MinimumPoints: 2, MaximumAge: 2 * time.Minute}, Window: 5 * time.Minute}
	config := HealthConfig{Targets: map[HealthTargetKey]HealthTarget{{"tenant-a", "ledger", "staging"}: target}}
	transport := &healthTransport{status: 200}
	reader, err := NewHealthReader(config, transport)
	if err != nil {
		t.Fatal(err)
	}
	reader.now = func() time.Time { return now }
	request := HealthRequest{TenantID: "tenant-a", Target: adapter.Target{Service: "ledger", Environment: "staging"}}
	transport.body = healthBody(t, now, target.Query.Expression, [][][]any{{{now.Add(-time.Minute).UnixMilli(), 0.2}, {now.UnixMilli(), 0.3}}})
	return reader, transport, config, request, now
}
func healthBody(t *testing.T, now time.Time, query string, points [][][]any) string {
	t.Helper()
	series := []map[string]any{}
	for _, p := range points {
		series = append(series, map[string]any{"pointlist": p})
	}
	data, err := json.Marshal(map[string]any{"status": "ok", "query": query, "from_date": now.Add(-5 * time.Minute).UnixMilli(), "to_date": now.UnixMilli(), "series": series})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDependencyHealthUsesFixedTenantSourceAndExportsNoSecrets(t *testing.T) {
	reader, transport, config, request, now := healthFixture(t)
	// Mutating caller-owned configuration must not redirect an existing reader.
	delete(config.Targets, HealthTargetKey{"tenant-a", "ledger", "staging"})
	got, err := reader.ReadHealth(context.Background(), request, datadogCredential(now))
	if err != nil || got.Status != adapter.VerificationPass {
		t.Fatalf("evidence=%+v err=%v", got, err)
	}
	r := transport.request
	if transport.calls != 1 || r.Method != http.MethodGet || r.URL.Host != "api.datadoghq.eu" || r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") != "max:dependency.error_rate{service:ledger,env:staging}" || r.Header.Get("DD-API-KEY") != "api-secret" || r.Header.Get("DD-APPLICATION-KEY") != "app-secret" || r.URL.Query().Get("from") != fmt.Sprint(now.Add(-5*time.Minute).Unix()) || r.URL.Query().Get("to") != fmt.Sprint(now.Unix()) {
		t.Fatalf("unexpected source request: %v", r)
	}
	if got.TenantID != request.TenantID || got.Target != request.Target || got.SchemaVersion != HealthSchema || got.SourceUpdatedAt != now || got.CapturedAt != now || got.EvidenceHash == "" || got.QueryHash == "" {
		t.Fatalf("evidence=%+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "dependency.error_rate") {
		t.Fatal("raw query/credential exported")
	}
	copied := got
	copied.EvidenceHash = ""
	hash, err := healthHash(copied)
	if err != nil || hash != got.EvidenceHash {
		t.Fatal("unreproducible evidence hash")
	}
	again, err := reader.ReadHealth(context.Background(), request, datadogCredential(now))
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatal("fixed input/time/source is not deterministic")
	}
	// Exported fields are a mutable DTO, never accepted as authenticated input.
	again.Status = adapter.VerificationFail
	if got.Status != adapter.VerificationPass {
		t.Fatal("result aliases caller state")
	}
}

func TestDependencyHealthPreservesFourStatesAndRejectsPartialSources(t *testing.T) {
	for _, name := range []string{"pass", "fail", "missing", "partial", "null", "malformed-point", "duplicate", "old-peer", "future-point", "stale", "future-only", "wrong-query", "wrong-window", "trailing-json", "oversized", "rate-limit", "unauthorized", "server-error", "timeout", "partial-series", "error-field"} {
		t.Run(name, func(t *testing.T) {
			reader, transport, _, request, now := healthFixture(t)
			expression := "max:dependency.error_rate{service:ledger,env:staging}"
			p := [][][]any{{{now.Add(-time.Minute).UnixMilli(), 0.2}, {now.UnixMilli(), 0.3}}}
			want := adapter.VerificationInconclusive
			reason := ""
			switch name {
			case "pass":
				want = adapter.VerificationPass
			case "fail":
				p[0][1][1] = 2.0
				want = adapter.VerificationFail
			case "missing":
				p = nil
				want = adapter.VerificationMissing
			case "partial":
				p[0] = p[0][:1]
			case "null":
				p[0][0][1] = nil
			case "malformed-point":
				p[0][0] = []any{now.UnixMilli()}
			case "duplicate":
				p[0][0][0] = now.UnixMilli()
			case "old-peer":
				p = append(p, [][]any{{now.Add(-5 * time.Minute).UnixMilli(), 0.1}, {now.Add(-4 * time.Minute).UnixMilli(), 0.1}})
			case "future-point":
				p[0][0][0] = now.Add(time.Second).UnixMilli()
			case "stale":
				p[0][1][0] = now.Add(-2 * time.Minute).UnixMilli()
				p[0][0][0] = now.Add(-3 * time.Minute).UnixMilli()
			case "future-only":
				p[0][0][0] = now.Add(time.Minute).UnixMilli()
				p[0][1][0] = now.Add(2 * time.Minute).UnixMilli()
			case "rate-limit":
				transport.status = 429
				reason = "HEALTH_SOURCE_RATE_LIMITED"
			case "unauthorized":
				transport.status = 403
				reason = "HEALTH_SOURCE_AUTH_REJECTED"
			case "server-error":
				transport.status = 503
			case "timeout":
				transport.err = errors.New("api-secret raw provider failure")
			case "partial-series":
				p = append(p, [][]any{})
			}
			transport.body = healthBody(t, now, expression, p)
			switch name {
			case "wrong-query":
				transport.body = strings.Replace(transport.body, expression, "other-query", 1)
			case "wrong-window":
				transport.body = strings.Replace(transport.body, fmt.Sprintf(`"to_date":%d`, now.UnixMilli()), `"to_date":1`, 1)
			case "trailing-json":
				transport.body += " {}"
			case "oversized":
				transport.body += strings.Repeat(" ", healthResponseLimit)
			case "error-field":
				transport.body = strings.Replace(transport.body, `"status":"ok"`, `"status":"ok","error":"api-secret"`, 1)
			}
			got, err := reader.ReadHealth(context.Background(), request, datadogCredential(now))
			if err != nil || got.Status != want || (reason != "" && got.ReasonCode != reason) {
				t.Fatalf("evidence=%+v err=%v want=%s", got, err, want)
			}
			raw, _ := json.Marshal(got)
			if got.EvidenceHash == "" || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "provider failure") {
				t.Fatal("missing hash or unsafe error data")
			}
		})
	}
}

func TestDependencyHealthRejectsUnknownTargetAndBadCredentialsBeforeHTTP(t *testing.T) {
	reader, transport, _, request, now := healthFixture(t)
	for _, req := range []HealthRequest{{TenantID: "tenant-b", Target: request.Target}, {TenantID: "tenant-a", Target: adapter.Target{Service: "other", Environment: "staging"}}, {TenantID: "tenant-a", Target: adapter.Target{Service: "ledger", Environment: "production"}}} {
		if got, err := reader.ReadHealth(context.Background(), req, datadogCredential(now)); !errors.Is(err, ErrHealthUnavailable) || got.EvidenceHash != "" {
			t.Fatal("unknown target accepted")
		}
	}
	for _, credential := range []credentials.Credential{{}, credentials.New(map[string][]byte{"api_key": []byte("secret"), "app_key": []byte("secret")}, now.Add(30*time.Second))} {
		if _, err := reader.ReadHealth(context.Background(), request, credential); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	if transport.calls != 0 {
		t.Fatal("untrusted request reached provider")
	}
}

func TestDependencyHealthCancelsAndFreshnessCannotBeExtendedByReadLatency(t *testing.T) {
	for _, mode := range []string{"cancel", "stale", "stale-negative", "credential-expired", "clock-regressed"} {
		t.Run(mode, func(t *testing.T) {
			reader, transport, _, request, now := healthFixture(t)
			if mode == "stale-negative" {
				transport.body = strings.Replace(transport.body, "0.3", "3.0", 1)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport.hook = func() {
				switch mode {
				case "cancel":
					cancel()
				case "stale", "stale-negative":
					reader.now = func() time.Time { return now.Add(2 * time.Minute) }
				case "credential-expired":
					reader.now = func() time.Time { return now.Add(time.Hour) }
				case "clock-regressed":
					reader.now = func() time.Time { return now.Add(-time.Second) }
				}
			}
			got, err := reader.ReadHealth(ctx, request, datadogCredential(now))
			if mode == "stale" || mode == "stale-negative" {
				if err != nil || got.Status != adapter.VerificationInconclusive || got.ReasonCode != "HEALTH_SOURCE_STALE" {
					t.Fatalf("%+v %v", got, err)
				}
				return
			}
			if !errors.Is(err, ErrHealthUnavailable) || got.EvidenceHash != "" {
				t.Fatalf("returned observation after %s: %+v %v", mode, got, err)
			}
		})
	}
}

func TestDependencyHealthValidatesConfigAndDefaultClient(t *testing.T) {
	_, _, config, _, _ := healthFixture(t)
	key := HealthTargetKey{"tenant-a", "ledger", "staging"}
	original := config.Targets[key]
	for _, mutate := range []func(*HealthTarget){func(v *HealthTarget) { v.Site = "http://arbitrary" }, func(v *HealthTarget) { v.Window = time.Minute + 1 }, func(v *HealthTarget) { v.Query.MaximumAge = v.Window + time.Second }, func(v *HealthTarget) { v.Query.Threshold = math.Inf(1) }, func(v *HealthTarget) { v.Query.MinimumPoints = 0 }, func(v *HealthTarget) { v.Query.Aggregation = "shell" }} {
		v := original
		mutate(&v)
		config.Targets[key] = v
		if _, err := NewHealthReader(config, nil); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	config.Targets[key] = original
	reader, err := NewHealthReader(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := reader.client.(*http.Client)
	if client.Timeout != 15*time.Second || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("unbounded/default redirecting client")
	}
}

func TestDependencyHealthCanonicalSampleHashAndPerSeriesFailure(t *testing.T) {
	reader, transport, _, request, now := healthFixture(t)
	expression := "max:dependency.error_rate{service:ledger,env:staging}"
	points := [][][]any{{{now.Add(-time.Minute).UnixMilli(), 0.1}, {now.UnixMilli(), 0.9}}, {{now.Add(-time.Minute).UnixMilli(), 0.2}, {now.UnixMilli(), 0.8}}}
	collect := func() HealthEvidence {
		t.Helper()
		transport.body = healthBody(t, now, expression, points)
		e, err := reader.ReadHealth(context.Background(), request, datadogCredential(now))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	first := collect()
	points[0], points[1] = points[1], points[0]
	points[0][0], points[0][1] = points[0][1], points[0][0]
	reordered := collect()
	if first.EvidenceHash != reordered.EvidenceHash || first.SamplesHash == "" || first.SampleCount != 4 {
		t.Fatal("sample order changes canonical observation")
	}
	points[1][0][1] = 0.3
	changed := collect()
	if first.SamplesHash == changed.SamplesHash || first.EvidenceHash == changed.EvidenceHash || first.QueryHash != changed.QueryHash {
		t.Fatal("sample content is not bound independently of query")
	}
	points[1][1][1] = 1.5
	if got := collect(); got.Status != adapter.VerificationFail {
		t.Fatal("healthy peer concealed failed series")
	}
}

func TestDependencyHealthLastAggregationUsesNewestPoint(t *testing.T) {
	reader, transport, _, request, now := healthFixture(t)
	key := HealthTargetKey{"tenant-a", "ledger", "staging"}
	target := reader.targets[key]
	target.Query.Aggregation = "last"
	reader.targets[key] = target
	transport.body = healthBody(t, now, target.Query.Expression, [][][]any{{{now.UnixMilli(), 0.1}, {now.Add(-time.Minute).UnixMilli(), 5.0}}})
	got, err := reader.ReadHealth(context.Background(), request, datadogCredential(now))
	if err != nil || got.Status != adapter.VerificationPass || got.ObservedValue != 0.1 {
		t.Fatalf("%+v %v", got, err)
	}
}
