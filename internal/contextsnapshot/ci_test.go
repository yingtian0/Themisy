package contextsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"themisy/adapters/githubactions"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

type ciTokenSource struct {
	calls  int
	expiry time.Time
	err    error
}

func (s *ciTokenSource) Token(context.Context) (credentials.Credential, error) {
	s.calls++
	return credentials.New(map[string][]byte{"token": []byte("credential-canary")}, s.expiry), s.err
}

type ciSourceHTTP struct {
	requests []*http.Request
	runBody  string
	status   int
	err      error
	commit   string
}

func (s *ciSourceHTTP) Do(r *http.Request) (*http.Response, error) {
	s.requests = append(s.requests, r.Clone(r.Context()))
	if s.err != nil {
		return nil, s.err
	}
	body := s.runBody
	if strings.Contains(r.URL.Path, "/artifacts") {
		body = `{"total_count":1,"artifacts":[{"id":81,"name":"build-archive","digest":"sha256:` + strings.Repeat("a", 64) + `","expired":false,"workflow_run":{"id":42,"head_sha":"` + s.commit + `"}}]}`
	}
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func ciCollectorFixture(t *testing.T) (*CICollector, *ciTokenSource, *ciSourceHTTP, githubactions.CIRequest, githubactions.CIConfig, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	commit := strings.Repeat("a", 40)
	config := githubactions.CIConfig{Targets: map[githubactions.CITargetKey]githubactions.CITarget{
		{TenantID: "tenant-a", Service: "payments", Environment: "staging"}: {TenantID: "tenant-a", Owner: "trusted-org", Repository: "payments", Workflow: "ci.yml", MaximumAge: time.Hour},
	}}
	source := &ciTokenSource{expiry: now.Add(time.Hour)}
	httpSource := &ciSourceHTTP{status: 200, commit: commit, runBody: `{"total_count":1,"workflow_runs":[{"id":42,"workflow_id":7,"head_sha":"` + commit + `","path":".github/workflows/ci.yml@main","status":"completed","conclusion":"success","run_attempt":1,"updated_at":"2026-10-19T23:59:00Z"}]}`}
	collector, err := NewCICollector(config, source, httpSource)
	if err != nil {
		t.Fatal(err)
	}
	collector.now = func() time.Time { return now }
	request := githubactions.CIRequest{TenantID: "tenant-a", Target: adapter.Target{Service: "payments", Environment: "staging"}, CommitSHA: commit}
	return collector, source, httpSource, request, config, now
}

func TestCICollectionPreservesBindingsAndDoesNotExportCredentials(t *testing.T) {
	collector, source, httpSource, request, _, now := ciCollectorFixture(t)
	snapshot, err := collector.Collect(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.RequireFreshCI(now); err != nil {
		t.Fatal(err)
	}
	r := snapshot.Record()
	if r.SchemaVersion != CISchema || r.TenantID != request.TenantID || r.Target != request.Target || r.CommitSHA != request.CommitSHA || r.Evidence.RunID != 42 || r.Evidence.Status != adapter.VerificationPass || r.Hash == "" || r.Hash != snapshot.Hash() || len(r.Evidence.Artifacts) != 1 {
		t.Fatalf("record=%+v", r)
	}
	if source.calls != 1 || len(httpSource.requests) != 2 {
		t.Fatal("unexpected credential/read count")
	}
	for _, r := range httpSource.requests {
		if r.Method != http.MethodGet || r.URL.Host != "api.github.com" || !strings.HasPrefix(r.URL.Path, "/repos/trusted-org/payments/actions/") {
			t.Fatalf("unexpected provider request: %s %s", r.Method, r.URL)
		}
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "credential-canary") {
		t.Fatal("credential reached snapshot")
	}
	// The provider fact is retained; neither source hash nor status is replaced.
	if r.Evidence.EvidenceHash == "" || r.Evidence.QueryHash == "" || r.Evidence.MaximumAge != time.Hour || r.Evidence.Repository != "trusted-org/payments" || r.Evidence.Artifacts[0].Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatal("source provenance lost")
	}
	before := snapshot.Hash()
	r.Evidence.Artifacts[0].Digest = "forged"
	r.Target.Service = "different"
	if snapshot.Hash() != before || snapshot.Record().Evidence.Artifacts[0].Digest == "forged" || snapshot.Record().Target.Service != "payments" {
		t.Fatal("exported record mutated snapshot")
	}
	if err := snapshot.RequireFreshCI(now.Add(59 * time.Minute)); err == nil {
		t.Fatal("source freshness deadline inclusive/extended")
	}
	if err := snapshot.RequireFreshCI(now.Add(-time.Second)); err == nil {
		t.Fatal("future observation accepted")
	}
}

func TestCISnapshotCannotBeReconstitutedFromUntrustedJSON(t *testing.T) {
	collector, _, _, request, _, now := ciCollectorFixture(t)
	snapshot, err := collector.Collect(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snapshot.Record())
	if err := json.Unmarshal(raw, &snapshot); !errors.Is(err, ErrUntrustedCIRecord) {
		t.Fatal("exported record imported as trusted")
	}
	if snapshot.Hash() != "" || snapshot.RequireFreshCI(now) == nil {
		t.Fatal("failed import retained positive cached value")
	}
	if (CISnapshot{}).RequireFreshCI(now) == nil {
		t.Fatal("zero snapshot accepted")
	}
}

func TestCICollectionRetainsFourStatesAndRefusesNonPass(t *testing.T) {
	for _, kind := range []string{"failure", "missing", "incomplete", "stale", "partial", "rate-limit", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			collector, _, httpSource, request, _, now := ciCollectorFixture(t)
			want := adapter.VerificationInconclusive
			switch kind {
			case "failure":
				httpSource.runBody = strings.Replace(httpSource.runBody, `"conclusion":"success"`, `"conclusion":"failure"`, 1)
				want = adapter.VerificationFail
			case "missing":
				httpSource.runBody = `{"total_count":0,"workflow_runs":[]}`
				want = adapter.VerificationMissing
			case "incomplete":
				httpSource.runBody = strings.Replace(httpSource.runBody, `"status":"completed"`, `"status":"in_progress"`, 1)
			case "stale":
				httpSource.runBody = strings.Replace(httpSource.runBody, "23:59:00Z", "20:00:00Z", 1)
			case "partial":
				httpSource.runBody = strings.Replace(httpSource.runBody, `"total_count":1`, `"total_count":2`, 1)
			case "rate-limit":
				httpSource.status = 429
			case "unavailable":
				httpSource.err = errors.New("credential-canary raw provider failure")
			}
			snapshot, err := collector.Collect(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Record().Evidence.Status != want || snapshot.Hash() == "" || snapshot.RequireFreshCI(now) == nil {
				t.Fatalf("status collapsed: %+v", snapshot.Record())
			}
			raw, _ := json.Marshal(snapshot.Record())
			if strings.Contains(string(raw), "credential-canary") {
				t.Fatal("raw provider error reached record")
			}
		})
	}
}

func TestCICollectionChecksAllowlistBeforeCredentialsAndDoesNotCacheFailures(t *testing.T) {
	for _, kind := range []string{"tenant", "service", "commit", "cancel", "config-mutation", "credential-failure"} {
		t.Run(kind, func(t *testing.T) {
			collector, source, httpSource, request, config, _ := ciCollectorFixture(t)
			ctx := context.Background()
			switch kind {
			case "tenant":
				request.TenantID = "tenant-b"
			case "service":
				request.Target.Service = "arbitrary"
			case "commit":
				request.CommitSHA = "not-a-commit"
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "config-mutation":
				key := githubactions.CITargetKey{TenantID: request.TenantID, Service: "new-service", Environment: request.Target.Environment}
				config.Targets[key] = githubactions.CITarget{Owner: "other", Repository: "other", Workflow: "other.yml", MaximumAge: time.Hour}
				request.Target.Service = "new-service"
			case "credential-failure":
				source.err = errors.New("credential-canary")
			}
			snapshot, err := collector.Collect(ctx, request)
			if !errors.Is(err, ErrCIContextUnavailable) || !reflect.DeepEqual(snapshot, CISnapshot{}) || len(httpSource.requests) != 0 {
				t.Fatalf("not refused: %+v %v", snapshot, err)
			}
			if kind != "credential-failure" && source.calls != 0 {
				t.Fatal("credential acquired for invalid target/request")
			}
		})
	}
}

func TestCISnapshotHashIsStableAndChangesWithContext(t *testing.T) {
	collector, _, httpSource, request, _, now := ciCollectorFixture(t)
	first, err := collector.Collect(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	upperCaseRequest := request
	upperCaseRequest.CommitSHA = strings.ToUpper(request.CommitSHA)
	second, err := collector.Collect(context.Background(), upperCaseRequest)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() != second.Hash() {
		t.Fatal("same exact observations produced different hashes")
	}
	httpSource.runBody = strings.Replace(httpSource.runBody, `"run_attempt":1`, `"run_attempt":2`, 1)
	changed, err := collector.Collect(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Hash() == first.Hash() {
		t.Fatal("changed source identity did not change snapshot")
	}
	changed.record.Hash = first.Hash()
	if changed.RequireFreshCI(now) == nil {
		t.Fatal("hash mismatch accepted")
	}
	badTimestamp := first.Record()
	badTimestamp.CapturedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if hash, err := hashRecord(badTimestamp); err == nil || hash != "" {
		t.Fatal("JSON serialization failure became a hash")
	}
	collector.now = func() time.Time { return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }
	if snapshot, err := collector.Collect(context.Background(), request); err == nil || snapshot.Hash() != "" {
		t.Fatal("invalid timestamp hashing returned a usable snapshot")
	}
}

func TestCICollectionDoesNotReturnEarlierPassAfterSourceFailure(t *testing.T) {
	collector, source, httpSource, request, _, now := ciCollectorFixture(t)
	first, err := collector.Collect(context.Background(), request)
	if err != nil || first.RequireFreshCI(now) != nil {
		t.Fatal("initial observation was not valid")
	}
	httpSource.err = errors.New("provider unavailable")
	later, err := collector.Collect(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if later.Record().Evidence.Status != adapter.VerificationInconclusive || later.RequireFreshCI(now) == nil || later.Hash() == first.Hash() || source.calls != 2 {
		t.Fatal("source failure reused earlier PASS")
	}
}

func TestCICollectorRejectsIncompleteTrustConfiguration(t *testing.T) {
	_, source, httpSource, _, config, _ := ciCollectorFixture(t)
	if _, err := NewCICollector(config, nil, httpSource); err == nil {
		t.Fatal("missing source accepted")
	}
	for key, target := range config.Targets {
		target.TenantID = "tenant-b"
		config.Targets[key] = target
	}
	if _, err := NewCICollector(config, source, httpSource); err == nil {
		t.Fatal("ambiguous tenant mapping accepted")
	}
	if source.calls != 0 || len(httpSource.requests) != 0 {
		t.Fatal("invalid configuration performed IO")
	}
}
