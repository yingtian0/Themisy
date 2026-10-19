package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/grant"
	policyfiles "themisy/policies"
)

func shadowFixture(t *testing.T) (Bundle, grant.Signer, StaticBundleKeys, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	signer, public, err := grant.NewDevelopmentSigner("shadow-key")
	if err != nil {
		t.Fatal(err)
	}
	source, err := policyfiles.FS.ReadFile("baseline/main.rego")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := SignBundle(context.Background(), signer, Bundle{Version: "current-v1", Issuer: "test-policy", Compatibility: []string{InputVersionV1Alpha1}, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Modules: []Module{{Name: "baseline.rego", Layer: LayerPlatform, Source: string(source)}}})
	if err != nil {
		t.Fatal(err)
	}
	return bundle, signer, StaticBundleKeys{"test-policy\x00shadow-key": public}, now
}
func signShadowCandidate(t *testing.T, signer grant.Signer, bundle Bundle, source string) Bundle {
	t.Helper()
	bundle.Version = "candidate-v2"
	bundle.Modules = append([]Module(nil), bundle.Modules...)
	bundle.Modules = append(bundle.Modules, Module{Name: "candidate.rego", Layer: LayerService, Source: source})
	signed, err := SignBundle(context.Background(), signer, bundle)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestShadowComparesSameInputWithoutApplyingCandidate(t *testing.T) {
	current, signer, keys, now := shadowFixture(t)
	candidate := signShadowCandidate(t, signer, current, `package themisy.authorization
import rego.v1
deny contains "SERVICE_FREEZE" if { input.service == "frozen" }`)
	cases := []ShadowCase{
		{ID: "unchanged", Input: Input{UserID: "synthetic-private-user", AgentID: "agent", Service: "stable", Environment: domain.EnvironmentStaging, ApprovalProofs: []string{"z", "a"}}},
		{ID: "candidate-deny", Input: Input{UserID: "user", AgentID: "agent", Service: "frozen", Environment: domain.EnvironmentStaging}},
		{ID: "existing-deny", Input: Input{AgentID: "agent", Service: "stable", Environment: domain.EnvironmentStaging}},
	}
	before, _ := json.Marshal([]any{current, candidate, cases})
	report, err := CompareSignedBundles(context.Background(), current, candidate, current.Issuer, keys, cases, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != ShadowReportVersion || report.CurrentHash != current.Hash || report.CandidateHash != candidate.Hash || report.ChangedCases != 1 || len(report.Cases) != 3 {
		t.Fatalf("report=%+v", report)
	}
	changed := report.Cases[1]
	if changed.Current.Decision != domain.DecisionAllow || changed.Candidate.Decision != domain.DecisionDeny || !changed.DecisionChanged || !reflect.DeepEqual(changed.AddedReasons, []string{"SERVICE_FREEZE"}) || !reflect.DeepEqual(changed.RemovedReasons, []string{"POLICY_ALLOW"}) {
		t.Fatalf("difference=%+v", changed)
	}
	if report.Cases[0].Changed || report.Cases[2].Changed || report.Cases[2].Candidate.Decision != domain.DecisionDeny {
		t.Fatal("unchanged or mandatory denial misreported")
	}
	for i, compared := range report.Cases {
		hash, _ := InputHash(cases[i].Input)
		if compared.InputHash != hash || compared.Current.InputHash != hash || compared.Candidate.InputHash != hash {
			t.Fatal("comparison used different canonical inputs")
		}
	}
	after, _ := json.Marshal([]any{current, candidate, cases})
	if string(before) != string(after) {
		t.Fatal("shadow mutated caller state")
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "synthetic-private-user") {
		t.Fatal("report exports raw input")
	}
	// The same signed bundle still evaluates with the current decision after shadow.
	active, err := NewVerifiedOPA(context.Background(), current, current.Issuer, keys, now)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := active.Evaluate(context.Background(), cases[1].Input)
	if err != nil || actual.Decision != domain.DecisionAllow {
		t.Fatal("candidate changed active policy")
	}
}

func TestShadowReportsRolesAndReasonOnlyChanges(t *testing.T) {
	current, signer, keys, now := shadowFixture(t)
	current.Modules = []Module{{Name: "approval.rego", Layer: LayerPlatform, Source: `package themisy.authorization
import rego.v1
decision := {"allow":false,"require_approval":true,"reasons":["REVIEW_REQUIRED"],"required_roles":["service-owner"]}`}}
	current, err := SignBundle(context.Background(), signer, current)
	if err != nil {
		t.Fatal(err)
	}
	candidate := current
	candidate.Version = "candidate-v2"
	candidate.Modules = []Module{{Name: "approval.rego", Layer: LayerPlatform, Source: `package themisy.authorization
import rego.v1
decision := {"allow":false,"require_approval":true,"reasons":["REVIEW_REQUIRED","CHANGE_WINDOW"],"required_roles":["sre","service-owner"]}`}}
	candidate, err = SignBundle(context.Background(), signer, candidate)
	if err != nil {
		t.Fatal(err)
	}
	report, err := CompareSignedBundles(context.Background(), current, candidate, current.Issuer, keys, []ShadowCase{{ID: "approval", Input: Input{}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	c := report.Cases[0]
	if c.DecisionChanged || !c.Changed || report.ChangedCases != 1 || !reflect.DeepEqual(c.AddedReasons, []string{"CHANGE_WINDOW"}) || !reflect.DeepEqual(c.AddedRequiredRoles, []string{"sre"}) || len(c.RemovedRequiredRoles) != 0 {
		t.Fatalf("comparison=%+v", c)
	}
}

func TestShadowDoesNotPresentFailedAnalysisAsNoChanges(t *testing.T) {
	for _, name := range []string{"signature", "expired", "future", "issuer", "keys", "compile", "evaluation", "case-id", "duplicate-id", "input-version", "empty", "limit", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			current, signer, keys, now := shadowFixture(t)
			candidate := current
			cases := []ShadowCase{{ID: "case", Input: Input{UserID: "user", AgentID: "agent"}}}
			issuer := current.Issuer
			ctx := context.Background()
			switch name {
			case "signature":
				candidate.Signature.Value = "invalid"
			case "expired":
				now = candidate.ExpiresAt
			case "future":
				now = candidate.IssuedAt.Add(-time.Second)
			case "issuer":
				issuer = "untrusted-policy"
			case "keys":
				keys = nil
			case "compile":
				candidate = signShadowCandidate(t, signer, current, "this is not Rego")
			case "evaluation":
				candidate.Modules = []Module{{Name: "bad.rego", Layer: LayerPlatform, Source: `package themisy.authorization
import rego.v1
decision := {"allow":true,"reasons":["invalid-reason"]}`}}
				var err error
				candidate, err = SignBundle(ctx, signer, candidate)
				if err != nil {
					t.Fatal(err)
				}
			case "case-id":
				cases[0].ID = " "
			case "duplicate-id":
				cases = append(cases, cases[0])
			case "input-version":
				cases[0].Input.Version = "future-policy-input"
			case "empty":
				cases = nil
			case "limit":
				cases = make([]ShadowCase, maximumShadowCases+1)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			report, err := CompareSignedBundles(ctx, current, candidate, issuer, keys, cases, now)
			if err == nil || !reflect.DeepEqual(report, ShadowReport{}) {
				t.Fatalf("failed analysis returned apparent success: err=%v report=%+v", err, report)
			}
		})
	}
}

func TestShadowRejectsNetworkAndNondeterministicBuiltins(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	for _, expression := range []string{
		fmt.Sprintf(`http.send({"method":"get","url":%q})`, server.URL),
		`net.lookup_ip_addr("example.invalid")`,
		`time.now_ns()`,
		`rand.intn("seed",10)`,
		`uuid.rfc4122("id")`,
		`opa.runtime()`,
	} {
		t.Run(expression, func(t *testing.T) {
			current, signer, keys, now := shadowFixture(t)
			candidate := signShadowCandidate(t, signer, current, "package themisy.authorization\nimport rego.v1\nside_effect := "+expression)
			report, err := CompareSignedBundles(context.Background(), current, candidate, current.Issuer, keys, []ShadowCase{{ID: "offline", Input: Input{}}}, now)
			if err == nil || len(report.Cases) != 0 {
				t.Fatal("offline shadow accepted restricted builtin")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("shadow made %d network calls", requests.Load())
	}
}

func TestShadowRevalidatesExpiryAndIgnoresMetadataOnlyChanges(t *testing.T) {
	current, signer, keys, now := shadowFixture(t)
	candidate := current
	candidate.Version = "metadata-v2"
	candidate, err := SignBundle(context.Background(), signer, candidate)
	if err != nil {
		t.Fatal(err)
	}
	cases := []ShadowCase{{ID: "same-decision", Input: Input{UserID: "user", AgentID: "agent"}}}
	report, err := CompareSignedBundles(context.Background(), current, candidate, current.Issuer, keys, cases, now)
	if err != nil {
		t.Fatal(err)
	}
	if current.Hash == candidate.Hash || report.ChangedCases != 0 || report.Cases[0].Changed {
		t.Fatal("metadata-only update reported authorization difference")
	}
	if report, err := CompareSignedBundles(context.Background(), current, candidate, current.Issuer, keys, cases, current.ExpiresAt); err == nil || len(report.Cases) != 0 {
		t.Fatal("expired bundles reused from previous comparison")
	}
}
