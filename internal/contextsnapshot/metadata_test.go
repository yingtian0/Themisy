package contextsnapshot

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/pkg/credentials"
)

type metadataHTTP struct {
	files    map[string]string
	requests []*http.Request
	mutate   func(map[string]any)
	hook     func()
	status   int
	err      error
}

func (h *metadataHTTP) Do(r *http.Request) (*http.Response, error) {
	h.requests = append(h.requests, r.Clone(r.Context()))
	if h.hook != nil {
		h.hook()
	}
	if h.err != nil {
		return nil, h.err
	}
	name := strings.TrimPrefix(r.URL.Path, "/repos/trusted-org/config/contents/")
	raw := h.files[name]
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d\x00", len(raw))
	_, _ = hash.Write([]byte(raw))
	item := map[string]any{"type": "file", "path": name, "encoding": "base64", "size": len(raw), "sha": hex.EncodeToString(hash.Sum(nil)), "content": base64.StdEncoding.EncodeToString([]byte(raw))}
	if h.mutate != nil {
		h.mutate(item)
	}
	body, _ := json.Marshal(item)
	return &http.Response{StatusCode: h.status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}
func metadataFixture(t *testing.T) (*MetadataCollector, *ciTokenSource, *metadataHTTP, *ciAuditSink, MetadataConfig, DependencyKey, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	contractData, e := os.ReadFile("../../examples/contracts/payment-api.yaml")
	if e != nil {
		t.Fatal(e)
	}
	profileData, e := os.ReadFile("../../examples/profiles/ecs-canary-critical.yaml")
	if e != nil {
		t.Fatal(e)
	}
	key := DependencyKey{"tenant-a", "payment-api", "staging"}
	config := MetadataConfig{Targets: map[DependencyKey]MetadataTarget{key: {Owner: "trusted-org", Repository: "config", Revision: strings.Repeat("a", 40), ContractPath: "contracts/payment-api.yaml", ProfilePath: "profiles/critical.yaml", MaximumAge: time.Hour}}}
	source := &ciTokenSource{expiry: now.Add(24 * time.Hour)}
	http := &metadataHTTP{files: map[string]string{"contracts/payment-api.yaml": string(contractData), "profiles/critical.yaml": string(profileData)}, status: 200}
	sink := &ciAuditSink{}
	c, e := NewMetadataCollector(config, source, http, sink)
	if e != nil {
		t.Fatal(e)
	}
	c.now = func() time.Time { return now }
	return c, source, http, sink, config, key, now
}
func TestMetadataCollectsPinnedValidatedSourcesAndAudits(t *testing.T) {
	c, source, http, sink, config, key, now := metadataFixture(t)
	config.Targets[key] = MetadataTarget{Owner: "attacker"}
	s, e := c.Collect(context.Background(), key)
	if e != nil || s.RequireFreshMetadata(now) != nil {
		t.Fatal("collection", e)
	}
	if source.calls != 1 || len(http.requests) != 2 || len(sink.events) != 1 {
		t.Fatal("unexpected fetch/audit counts")
	}
	for _, r := range http.requests {
		if r.Method != "GET" || r.URL.Host != "api.github.com" || !strings.HasPrefix(r.URL.Path, "/repos/trusted-org/config/contents/") || r.URL.Query().Get("ref") != strings.Repeat("a", 40) || len(r.URL.Query()) != 1 || r.Header.Get("Authorization") != "Bearer credential-canary" {
			t.Fatal("source redirected", r.URL)
		}
	}
	r := s.Record()
	if r.SchemaVersion != MetadataSchema || r.TenantID != key.TenantID || r.Service != key.Service || r.Environment != key.Environment || r.Contract.Kind != "ServiceContract" || r.Profile.Kind != "ReleaseProfile" || r.Contract.Revision != strings.Repeat("a", 40) || r.Contract.ContentHash == "" || r.Contract.RawHash == "" || r.Profile.ContentHash == "" {
		t.Fatalf("missing provenance %+v", r)
	}
	h, e := metadataHash(r)
	if e != nil || h != s.Hash() || sink.events[0].ResourceID != h || !reflect.DeepEqual(sink.events[0].Details["record"], r) {
		t.Fatal("hash/audit mismatch")
	}
	raw, _ := json.Marshal(sink.events[0])
	if strings.Contains(string(raw), "credential-canary") || strings.Contains(string(raw), "payment-team") || strings.Contains(string(raw), "runnerGroup") {
		t.Fatal("secret/raw document in audit")
	}
	contractValue, e := s.Contract()
	if e != nil || contractValue.Source != "github:trusted-org/config@"+strings.Repeat("a", 40)+":contracts/payment-api.yaml" {
		t.Fatal("contract source", e)
	}
	environment := contractValue.Environments["staging"]
	environment.RunnerGroup = "mutated"
	contractValue.Environments["staging"] = environment
	contractValue.Verification.Checks[0].Type = "mutated"
	again, _ := s.Contract()
	if again.Verification.Checks[0].Type == "mutated" || again.Environments["staging"].RunnerGroup == "mutated" {
		t.Fatal("mutable contract alias")
	}
	profileValue, _ := s.Profile()
	profileValue.Spec.RequiredCapabilities[0] = "mutated"
	profileAgain, _ := s.Profile()
	if profileAgain.Spec.RequiredCapabilities[0] == "mutated" {
		t.Fatal("mutable profile alias")
	}
	r.Contract.Repository = "attacker"
	if s.Record().Contract.Repository != "trusted-org/config" {
		t.Fatal("mutable record alias")
	}
	another, e := c.Collect(context.Background(), key)
	if e != nil || another.Hash() != s.Hash() {
		t.Fatal("fixed observations not deterministic")
	}
	var restored MetadataSnapshot
	if json.Unmarshal(raw, &restored) == nil || restored.RequireFreshMetadata(now) == nil || (MetadataSnapshot{}).RequireFreshMetadata(now) == nil {
		t.Fatal("JSON/zero minted trust")
	}
	if s.RequireFreshMetadata(now.Add(time.Hour)) == nil || s.RequireFreshMetadata(now.Add(time.Hour-time.Nanosecond)) != nil || s.RequireFreshMetadata(now.Add(-time.Second)) == nil {
		t.Fatal("freshness boundary")
	}
}
func TestMetadataRejectsUntrustedSelectorsBeforeCredentialAcquisition(t *testing.T) {
	for _, field := range []string{"tenant", "service", "environment"} {
		t.Run(field, func(t *testing.T) {
			c, source, http, sink, _, key, _ := metadataFixture(t)
			switch field {
			case "tenant":
				key.TenantID = "other"
			case "service":
				key.Service = "other"
			case "environment":
				key.Environment = "other"
			}
			if s, e := c.Collect(context.Background(), key); e == nil || s.Hash() != "" || source.calls != 0 || len(http.requests) != 0 || len(sink.events) != 0 {
				t.Fatal("untrusted selector fetched")
			}
		})
	}
	for _, bad := range []string{"../outside", "/absolute", "a/../b", "a//b", "a/./b", "a?x=y", "a#fragment", "a%2fb", "a\\b"} {
		t.Run(bad, func(t *testing.T) {
			_, source, http, sink, config, key, _ := metadataFixture(t)
			v := config.Targets[key]
			v.ContractPath = bad
			config.Targets[key] = v
			if _, e := NewMetadataCollector(config, source, http, sink); e == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	for _, bad := range []string{"main", "v1.0.0", strings.Repeat("A", 40), strings.Repeat("a", 39)} {
		t.Run(bad, func(t *testing.T) {
			_, source, http, sink, config, key, _ := metadataFixture(t)
			v := config.Targets[key]
			v.Revision = bad
			config.Targets[key] = v
			if _, e := NewMetadataCollector(config, source, http, sink); e == nil {
				t.Fatal("mutable/invalid revision accepted")
			}
		})
	}
}
func TestMetadataRejectsInvalidProviderAndDocuments(t *testing.T) {
	for _, mode := range []string{"timeout", "partial", "rate-limit", "redirect", "missing", "symlink", "submodule", "wrong-path", "wrong-size", "bad-encoding", "bad-base64", "wrong-blob", "oversize", "malformed-schema", "wrong-service", "wrong-environment", "wrong-profile", "missing-capability", "unknown-field", "multi-document"} {
		t.Run(mode, func(t *testing.T) {
			c, _, http, sink, _, key, _ := metadataFixture(t)
			switch mode {
			case "timeout":
				http.err = errors.New("credential-canary raw error")
			case "partial":
				http.hook = func() {
					if len(http.requests) == 2 {
						http.status = 503
					}
				}
			case "rate-limit":
				http.status = 429
			case "redirect":
				http.status = 302
			case "missing":
				http.status = 404
			case "symlink":
				http.mutate = func(m map[string]any) { m["type"] = "symlink" }
			case "submodule":
				http.mutate = func(m map[string]any) { m["submodule_git_url"] = "https://attacker" }
			case "wrong-path":
				http.mutate = func(m map[string]any) { m["path"] = "wrong" }
			case "wrong-size":
				http.mutate = func(m map[string]any) { m["size"] = 1 }
			case "bad-encoding":
				http.mutate = func(m map[string]any) { m["encoding"] = "none" }
			case "bad-base64":
				http.mutate = func(m map[string]any) { m["content"] = "!!" }
			case "wrong-blob":
				http.mutate = func(m map[string]any) { m["sha"] = strings.Repeat("b", 40) }
			case "oversize":
				http.mutate = func(m map[string]any) { m["content"] = strings.Repeat("a", 3*metadataLimit) }
			case "malformed-schema":
				http.files["contracts/payment-api.yaml"] = strings.Replace(http.files["contracts/payment-api.yaml"], "v1alpha1", "v999", 1)
			case "wrong-service":
				http.files["contracts/payment-api.yaml"] = strings.Replace(http.files["contracts/payment-api.yaml"], "name: payment-api", "name: other", 1)
			case "wrong-environment":
				key.Environment = "qa"
				c.targets[key] = c.targets[DependencyKey{"tenant-a", "payment-api", "staging"}]
				c.broker.Allowed[credentials.GitHubTarget{TenantID: key.TenantID, Service: key.Service, Environment: key.Environment}] = struct{}{}
			case "wrong-profile":
				http.files["profiles/critical.yaml"] = strings.Replace(http.files["profiles/critical.yaml"], "name: ecs-canary-critical", "name: other", 1)
			case "missing-capability":
				http.files["profiles/critical.yaml"] = strings.Replace(http.files["profiles/critical.yaml"], "    - deploy", "    - cancel", 1)
			case "unknown-field":
				http.files["contracts/payment-api.yaml"] += "\nunknown: true\n"
			case "multi-document":
				http.files["profiles/critical.yaml"] += "\n---\nsecret: value\n"
			}
			s, e := c.Collect(context.Background(), key)
			if e == nil || e.Error() != ErrMetadataContext.Error() || s.Hash() != "" || len(sink.events) != 0 {
				t.Fatal("invalid input returned snapshot or raw diagnostics", e)
			}
		})
	}
}
func TestMetadataRejectsCredentialAuditAndCancellationFailures(t *testing.T) {
	for _, mode := range []string{"credential", "expired", "cancel-before", "cancel-fetch", "audit", "audit-cancel", "audit-expiry", "clock-reversal", "fetch-expiry"} {
		t.Run(mode, func(t *testing.T) {
			c, source, http, sink, _, key, now := metadataFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "credential":
				source.err = errors.New("credential-canary")
			case "expired":
				source.expiry = now
			case "cancel-before":
				cancel()
			case "cancel-fetch":
				http.hook = cancel
			case "audit":
				sink.err = errors.New("response lost credential-canary")
			case "audit-cancel":
				sink.hook = func(context.Context, domain.AuditEvent) { cancel() }
			case "audit-expiry":
				sink.hook = func(context.Context, domain.AuditEvent) { c.now = func() time.Time { return now.Add(time.Hour) } }
			case "clock-reversal":
				http.hook = func() { c.now = func() time.Time { return now.Add(-time.Second) } }
			case "fetch-expiry":
				http.hook = func() { c.now = func() time.Time { return now.Add(25 * time.Hour) } }
			}
			if s, e := c.Collect(ctx, key); e == nil || s.Hash() != "" {
				t.Fatal("failure returned snapshot")
			}
		})
	}
}

func TestMetadataBindsRawContentSeparatelyFromCanonicalContent(t *testing.T) {
	c, _, http, _, _, key, _ := metadataFixture(t)
	first, err := c.Collect(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	http.files["contracts/payment-api.yaml"] += "\n# new source bytes, same semantics\n"
	second, err := c.Collect(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	a, b := first.Record(), second.Record()
	if a.Contract.ContentHash != b.Contract.ContentHash || a.Contract.RawHash == b.Contract.RawHash || a.Contract.BlobSHA == b.Contract.BlobSHA || first.Hash() == second.Hash() {
		t.Fatal("raw and canonical provenance not bound independently")
	}
}
