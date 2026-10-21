package githubactions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

type ciHTTPDoer struct {
	responses []*http.Response
	requests  []*http.Request
	err       error
}

func (d *ciHTTPDoer) Do(request *http.Request) (*http.Response, error) {
	d.requests = append(d.requests, request.Clone(request.Context()))
	if d.err != nil {
		return nil, d.err
	}
	if len(d.responses) == 0 {
		return nil, errors.New("unexpected request")
	}
	response := d.responses[0]
	d.responses = d.responses[1:]
	return response, nil
}

func ciHTTPResponse(status int, body string, requestURL string) *http.Response {
	var request *http.Request
	if requestURL != "" {
		request, _ = http.NewRequest(http.MethodGet, requestURL, nil)
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func ciFixture(t *testing.T, runBody, artifactBody string, status int) (*CIAdapter, *ciHTTPDoer, CIRequest, credentials.Credential, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	commit := strings.Repeat("a", 40)
	doer := &ciHTTPDoer{responses: []*http.Response{ciHTTPResponse(status, runBody, ""), ciHTTPResponse(http.StatusOK, artifactBody, "")}}
	config := CIConfig{Targets: map[CITargetKey]CITarget{{TenantID: "tenant-1", Service: "payments", Environment: "staging"}: {TenantID: "tenant-1", Owner: "example-org", Repository: "payments", Workflow: "ci.yml", MaximumAge: time.Hour}}}
	client, err := NewCIAdapter(config, doer)
	if err != nil {
		t.Fatal(err)
	}
	client.Now = func() time.Time { return now }
	request := CIRequest{TenantID: "tenant-1", Target: adapter.Target{Service: "payments", Environment: "staging"}, CommitSHA: commit}
	credential := credentials.New(map[string][]byte{"token": []byte("read-token")}, now.Add(time.Hour))
	return client, doer, request, credential, now
}

func ciRunBody(commit, status, conclusion string) string {
	return `{"total_count":1,"workflow_runs":[{"id":42,"workflow_id":7,"head_sha":"` + commit + `","path":".github/workflows/ci.yml@main","status":"` + status + `","conclusion":"` + conclusion + `","run_attempt":2,"updated_at":"2026-10-19T11:59:00Z"}]}`
}

func ciArtifactBody(commit string) string {
	return `{"total_count":1,"artifacts":[{"id":81,"name":"release-bundle","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expired":false,"workflow_run":{"id":42,"head_sha":"` + commit + `"}}]}`
}

func TestReadCIRequiresExactCommitAndPersistsArtifactArchiveDigest(t *testing.T) {
	commit := strings.Repeat("a", 40)
	client, doer, request, credential, now := ciFixture(t, ciRunBody(commit, "completed", "success"), ciArtifactBody(commit), http.StatusOK)
	evidence, err := client.ReadCI(context.Background(), request, credential)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != adapter.VerificationPass || evidence.ReasonCode != "CI_RUN_SUCCEEDED" || evidence.Repository != "example-org/payments" || evidence.CommitSHA != commit || evidence.RunID != 42 || evidence.RunAttempt != 2 || evidence.QueryHash == "" || evidence.EvidenceHash == "" || !evidence.ObservedAt.Equal(now) || evidence.Freshness != time.Minute || evidence.MaximumAge != time.Hour {
		t.Fatalf("evidence=%#v", evidence)
	}
	if len(evidence.Artifacts) != 1 || evidence.Artifacts[0].ID != 81 || evidence.Artifacts[0].Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("artifacts=%#v", evidence.Artifacts)
	}
	if hash := hashCIEvidence(evidence); hash != evidence.EvidenceHash {
		t.Fatalf("hash=%s want %s", hash, evidence.EvidenceHash)
	}
	if len(doer.requests) != 2 {
		t.Fatalf("requests=%d", len(doer.requests))
	}
	for _, request := range doer.requests {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer read-token" || request.Header.Get("X-GitHub-Api-Version") != apiVersion {
			t.Fatalf("request method/header: %s %#v", request.Method, request.Header)
		}
	}
	parsed, err := url.Parse(doer.requests[0].URL.String())
	if err != nil || parsed.Query().Get("head_sha") != commit {
		t.Fatalf("selector=%v err=%v", parsed, err)
	}
}

func TestReadCIFailsClosedForMissingFailedIncompleteAndPartial(t *testing.T) {
	commit := strings.Repeat("a", 40)
	cases := []struct {
		name, runBody, artifacts string
		want                     adapter.VerificationStatus
		reason                   string
	}{
		{name: "missing", runBody: `{"total_count":0,"workflow_runs":[]}`, artifacts: "", want: adapter.VerificationMissing, reason: "CI_RUN_MISSING"},
		{name: "failed", runBody: ciRunBody(commit, "completed", "failure"), artifacts: "", want: adapter.VerificationFail, reason: "CI_RUN_FAILED"},
		{name: "incomplete", runBody: ciRunBody(commit, "in_progress", ""), artifacts: "", want: adapter.VerificationInconclusive, reason: "CI_RUN_INCOMPLETE"},
		{name: "wrong commit", runBody: ciRunBody(strings.Repeat("b", 40), "completed", "success"), artifacts: "", want: adapter.VerificationInconclusive, reason: "CI_RUN_IDENTITY_MISMATCH"},
		{name: "partial list", runBody: strings.Replace(ciRunBody(commit, "completed", "success"), `"total_count":1`, `"total_count":101`, 1), artifacts: "", want: adapter.VerificationInconclusive, reason: "CI_RUN_LIST_PARTIAL"},
		{name: "inconsistent empty list", runBody: `{"total_count":1,"workflow_runs":[]}`, artifacts: "", want: adapter.VerificationInconclusive, reason: "CI_RUN_LIST_PARTIAL"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			client, _, request, credential, _ := ciFixture(t, test.runBody, test.artifacts, http.StatusOK)
			evidence, err := client.ReadCI(context.Background(), request, credential)
			if err != nil || evidence.Status != test.want || evidence.ReasonCode != test.reason {
				t.Fatalf("evidence=%#v err=%v", evidence, err)
			}
		})
	}
}

func TestReadCIRequiresArtifactRunBindingAndMapsProviderFailures(t *testing.T) {
	commit := strings.Repeat("a", 40)
	badArtifact := strings.Replace(ciArtifactBody(commit), `"id":42`, `"id":43`, 1)
	client, _, request, credential, _ := ciFixture(t, ciRunBody(commit, "completed", "success"), badArtifact, http.StatusOK)
	evidence, err := client.ReadCI(context.Background(), request, credential)
	if err != nil || evidence.Status != adapter.VerificationInconclusive || evidence.ReasonCode != "ARTIFACT_IDENTITY_MISMATCH" {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}

	for _, tc := range []struct {
		status int
		reason string
	}{{http.StatusForbidden, "SOURCE_UNAUTHORIZED"}, {http.StatusTooManyRequests, "SOURCE_RATE_LIMITED"}, {http.StatusServiceUnavailable, "SOURCE_UNAVAILABLE"}} {
		doer := &ciHTTPDoer{responses: []*http.Response{ciHTTPResponse(tc.status, "{}", "")}}
		client, err := NewCIAdapter(CIConfig{Targets: map[CITargetKey]CITarget{{TenantID: "tenant-1", Service: "payments", Environment: "staging"}: {TenantID: "tenant-1", Owner: "example-org", Repository: "payments", Workflow: "ci.yml", MaximumAge: time.Hour}}}, doer)
		if err != nil {
			t.Fatal(err)
		}
		client.Now = func() time.Time { return time.Now().UTC() }
		evidence, err := client.ReadCI(context.Background(), CIRequest{TenantID: "tenant-1", Target: adapter.Target{Service: "payments", Environment: "staging"}, CommitSHA: commit}, credentials.New(map[string][]byte{"token": []byte("token")}, time.Now().Add(time.Hour)))
		if err != nil || evidence.Status != adapter.VerificationInconclusive || evidence.ReasonCode != tc.reason {
			t.Fatalf("status=%d evidence=%#v err=%v", tc.status, evidence, err)
		}
	}
}

func TestReadCIMissingArtifactDoesNotPass(t *testing.T) {
	commit := strings.Repeat("a", 40)
	client, _, request, credential, _ := ciFixture(t, ciRunBody(commit, "completed", "success"), `{"total_count":0,"artifacts":[]}`, http.StatusOK)
	evidence, err := client.ReadCI(context.Background(), request, credential)
	if err != nil || evidence.Status != adapter.VerificationMissing || evidence.ReasonCode != "CI_ARTIFACT_MISSING" {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
}

func TestReadCIDoesNotUseExpiredCredentialsOrFollowRedirects(t *testing.T) {
	commit := strings.Repeat("a", 40)
	client, doer, request, credential, now := ciFixture(t, ciRunBody(commit, "completed", "success"), ciArtifactBody(commit), http.StatusOK)
	if _, err := client.ReadCI(context.Background(), request, credentials.New(map[string][]byte{"token": []byte("expired")}, now)); err == nil || len(doer.requests) != 0 {
		t.Fatal("expired token reached provider")
	}

	redirectTarget := apiBaseURL + "/redirected"
	doer.responses = []*http.Response{ciHTTPResponse(http.StatusMovedPermanently, "", redirectTarget)}
	evidence, err := client.ReadCI(context.Background(), request, credential)
	if err != nil || evidence.Status != adapter.VerificationInconclusive || evidence.ReasonCode != "SOURCE_UNAVAILABLE" {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
	if len(doer.requests) != 1 || doer.requests[0].Method != http.MethodGet {
		t.Fatalf("requests=%d", len(doer.requests))
	}
}

func TestCIConfigRejectsUntrustedWorkflowAndRequestCannotSelectRepository(t *testing.T) {
	if err := (CIConfig{Targets: map[CITargetKey]CITarget{{TenantID: "tenant-1", Service: "x", Environment: "prod"}: {TenantID: "tenant-1", Owner: "example", Repository: "repo", Workflow: "../../write.yml", MaximumAge: time.Hour}}}).Validate(); err == nil {
		t.Fatal("path workflow accepted")
	}
	client, doer, request, credential, _ := ciFixture(t, `{"total_count":0,"workflow_runs":[]}`, "", http.StatusOK)
	request.CommitSHA = "abc"
	if _, err := client.ReadCI(context.Background(), request, credential); err == nil || len(doer.requests) != 0 {
		t.Fatal("invalid revision reached provider")
	}
	request.CommitSHA = strings.Repeat("a", 40)
	request.TenantID = "tenant-other"
	if _, err := client.ReadCI(context.Background(), request, credential); err == nil || len(doer.requests) != 0 {
		t.Fatal("tenant without an allowlisted CI target reached provider")
	}
}

func TestCIEvidenceJSONRemainsTyped(t *testing.T) {
	data, err := json.Marshal(CIEvidence{SchemaVersion: ciEvidenceSchema, Status: adapter.VerificationMissing, ReasonCode: "CI_RUN_MISSING"})
	if err != nil || !strings.Contains(string(data), `"status":"MISSING"`) {
		t.Fatalf("json=%s err=%v", data, err)
	}
}

func TestReadCIRejectsStaleSuccessfulRun(t *testing.T) {
	commit := strings.Repeat("a", 40)
	run := strings.Replace(ciRunBody(commit, "completed", "success"), "2026-10-19T11:59:00Z", "2026-10-17T11:59:00Z", 1)
	client, _, request, credential, _ := ciFixture(t, run, ciArtifactBody(commit), http.StatusOK)
	evidence, err := client.ReadCI(context.Background(), request, credential)
	if err != nil || evidence.Status != adapter.VerificationInconclusive || evidence.ReasonCode != "CI_RUN_STALE" || evidence.Freshness <= evidence.MaximumAge {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
}
