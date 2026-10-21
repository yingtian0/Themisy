package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"themisy/internal/application"
	"themisy/internal/grant"
	"themisy/internal/resultreport"
	"themisy/internal/store"
	"themisy/pkg/adapter"
	"themisy/pkg/protocol"
	"time"
)

type reportTestKey struct{ rotated bool }

func (k reportTestKey) seed() []byte {
	seed := make([]byte, 32)
	if k.rotated {
		seed[0] = 1
	}
	return seed
}
func (k reportTestKey) id() string {
	if k.rotated {
		return "report-rotated"
	}
	return "report-test"
}

func (k reportTestKey) Sign(_ context.Context, p []byte) (protocol.Signature, error) {
	return protocol.Signature{KeyID: k.id(), Algorithm: "Ed25519", Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(k.seed()), p))}, nil
}
func (reportTestKey) ResolveResultKey(_ context.Context, _ resultreport.Identity, id string) (ed25519.PublicKey, error) {
	if id != "report-test" && id != "report-rotated" {
		return nil, fmt.Errorf("unknown result key")
	}
	return ed25519.NewKeyFromSeed((reportTestKey{rotated: id == "report-rotated"}).seed()).Public().(ed25519.PublicKey), nil
}
func testReportSigner() *resultreport.Signer {
	return &resultreport.Signer{Signer: reportTestKey{}, KeyID: "report-test"}
}
func testReportVerifier() *resultreport.Verifier {
	return &resultreport.Verifier{Keys: reportTestKey{}}
}

func TestSignedReportEndpointsRejectBeforeMutation(t *testing.T) {
	for _, endpoint := range []string{"result", "reconcile"} {
		for _, negative := range []string{"unsigned", "tampered", "nil-verifier", "wrong-runner", "wrong-tenant", "wrong-group", "wrong-grant", "wrong-key"} {
			t.Run(endpoint+"/"+negative, func(t *testing.T) {
				ctx := context.Background()
				now := time.Now().UTC()
				memory := store.NewMemory()
				run := approvedTransportRun(now)
				memory.CreateRun(run)
				grantSigner, _, _ := grant.NewDevelopmentSigner("grant")
				issuer, _ := application.NewGrants(memory, grantSigner, "https://control.example", time.Minute)
				issued, _, err := issuer.Issue(ctx, application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
				if err != nil {
					t.Fatal(err)
				}
				memory.ClaimGrantDispatch(ctx, issued.Grant.TenantID, issued.Grant.RunnerGroup, "runner-1", "delivery-secret", now, now.Add(time.Minute))
				before, _ := memory.GetGrantDispatch(ctx, issued.Grant.GrantID)
				result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: issued.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "provider", CompletedAt: now}
				hash, _ := protocol.GrantHash(issued.Grant)
				report, err := testReportSigner().Sign(ctx, resultreport.Identity{RunnerID: "runner-1", TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup}, hash, result)
				if err != nil {
					t.Fatal(err)
				}
				verifier := testReportVerifier()
				switch negative {
				case "unsigned":
					report.Signature = protocol.Signature{}
				case "tampered":
					report.Result.ExternalExecutionID = "forged"
				case "nil-verifier":
					verifier = nil
				case "wrong-runner":
					report.RunnerID = "other"
				case "wrong-tenant":
					report.TenantID = "other"
				case "wrong-group":
					report.RunnerGroup = "other"
				case "wrong-grant":
					report.Result.GrantID = "other"
				case "wrong-key":
					report.Signature.KeyID = "other"
				}
				var body any = resultSubmission{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, DeliveryToken: "delivery-secret", Result: result, Report: report}
				if endpoint == "reconcile" {
					body = recoveryReport{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, RequestHash: hash, Result: result, Report: report}
				}
				raw, _ := json.Marshal(body)
				request := httptest.NewRequest(http.MethodPost, "/v1/runner/grants/"+issued.Grant.GrantID+"/"+endpoint, bytes.NewReader(raw))
				request.Header.Set(runnerIDHeader, "runner-1")
				request.Header.Set("Authorization", "Bearer auth-secret")
				response := httptest.NewRecorder()
				server := &RunnerServer{Store: memory, Audit: memory, ResultVerifier: verifier, Auth: StaticRunnerAuthenticator{"runner-1": {TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup, Token: "auth-secret"}}}
				server.Handler().ServeHTTP(response, request)
				after, _ := memory.GetGrantDispatch(ctx, issued.Grant.GrantID)
				if response.Code != http.StatusForbidden || !reflect.DeepEqual(before, after) {
					t.Fatalf("status=%d state changed=%v", response.Code, !reflect.DeepEqual(before, after))
				}
				events, _ := memory.AuditEvents(run.ID)
				last := events[len(events)-1]
				encoded, _ := json.Marshal(last)
				if last.Result != "REJECTED" || bytes.Contains(encoded, []byte("delivery-secret")) || bytes.Contains(encoded, []byte("auth-secret")) || bytes.Contains(encoded, []byte("forged")) {
					t.Fatal("missing or unsanitized rejection audit")
				}
			})
		}
	}
}

func TestSignedEvidenceBodyIsVerifiedBeforeDurableResult(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	memory := store.NewMemory()
	run := approvedTransportRun(now)
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	grantSigner, _, _ := grant.NewDevelopmentSigner("grant")
	issuer, _ := application.NewGrants(memory, grantSigner, "https://control.example", time.Minute)
	issued, _, err := issuer.Issue(ctx, application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.ClaimGrantDispatch(ctx, issued.Grant.TenantID, issued.Grant.RunnerGroup, "runner-1", "delivery-secret", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	evidence := adapter.Evidence{Status: adapter.VerificationPass, ReasonCode: "THRESHOLD_MET", Source: "datadog", QueryHash: "sha256:query", Window: adapter.ObservationWindow{From: now.Add(-time.Minute), To: now}, ObservedAt: now, ObservedValue: 0.2, Threshold: 1, AdapterVersion: "datadog/v1"}
	evidence.EvidenceHash, err = adapter.EvidenceHash(evidence)
	if err != nil {
		t.Fatal(err)
	}
	result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: issued.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "provider", CompletedAt: now, EvidenceHash: evidence.EvidenceHash, Evidence: &evidence}
	hash, _ := protocol.GrantHash(issued.Grant)
	identity := resultreport.Identity{RunnerID: "runner-1", TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup}
	report, err := testReportSigner().Sign(ctx, identity, hash, result)
	if err != nil {
		t.Fatal(err)
	}
	tampered := report
	tamperedEvidence := *report.Result.Evidence
	tamperedEvidence.ObservedValue = 0.3
	tamperedEvidence.EvidenceHash, err = adapter.EvidenceHash(tamperedEvidence)
	if err != nil {
		t.Fatal(err)
	}
	tampered.Result.Evidence = &tamperedEvidence
	tampered.Result.EvidenceHash = tamperedEvidence.EvidenceHash
	tamperedResult := tampered.Result
	post := func(result protocol.Result, signed protocol.ResultReport) *httptest.ResponseRecorder {
		body, _ := json.Marshal(resultSubmission{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, DeliveryToken: "delivery-secret", Result: result, Report: signed})
		request := httptest.NewRequest(http.MethodPost, "/v1/runner/grants/"+issued.Grant.GrantID+"/result", bytes.NewReader(body))
		request.Header.Set(runnerIDHeader, "runner-1")
		request.Header.Set("Authorization", "Bearer auth-secret")
		response := httptest.NewRecorder()
		(&RunnerServer{Store: memory, Audit: memory, ResultVerifier: testReportVerifier(), Auth: StaticRunnerAuthenticator{"runner-1": {TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup, Token: "auth-secret"}}}).Handler().ServeHTTP(response, request)
		return response
	}
	if response := post(tamperedResult, tampered); response.Code != http.StatusForbidden {
		t.Fatalf("tampered body status=%d, want forbidden", response.Code)
	}
	unchanged, _ := memory.GetGrantDispatch(ctx, issued.Grant.GrantID)
	if unchanged.Status != store.GrantDispatchLeased || unchanged.Result.Evidence != nil {
		t.Fatal("tampered evidence changed durable dispatch state")
	}
	if response := post(result, report); response.Code != http.StatusNoContent {
		t.Fatalf("valid signed evidence status=%d body=%s", response.Code, response.Body.String())
	}
	completed, err := memory.GetGrantDispatch(ctx, issued.Grant.GrantID)
	if err != nil || completed.Status != store.GrantDispatchSucceeded || !protocol.ResultEqual(completed.Result, result) {
		t.Fatalf("signed evidence was not durably stored: status=%s err=%v", completed.Status, err)
	}
}

// Real file trust is reloaded through both HTTP endpoints, including retries.
func TestReportEndpointsReloadResultTrustRevocation(t *testing.T) {
	for _, endpoint := range []string{"result", "reconcile"} {
		t.Run(endpoint, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			memory := store.NewMemory()
			run := approvedTransportRun(now)
			memory.CreateRun(run)
			grantSigner, _, _ := grant.NewDevelopmentSigner("grant")
			issuer, _ := application.NewGrants(memory, grantSigner, "https://control.example", time.Minute)
			issued, _, err := issuer.Issue(ctx, application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := memory.ClaimGrantDispatch(ctx, issued.Grant.TenantID, issued.Grant.RunnerGroup, "runner-1", "delivery", now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			identity := resultreport.Identity{RunnerID: "runner-1", TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup}
			signer, public, _ := grant.NewDevelopmentSigner("result-key")
			hash, _ := protocol.GrantHash(issued.Grant)
			result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: issued.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "provider", CompletedAt: now}
			report, err := (&resultreport.Signer{Signer: signer, KeyID: "result-key"}).Sign(ctx, identity, hash, result)
			if err != nil {
				t.Fatal(err)
			}
			trust := resultreport.ResultTrustBundle{Version: "themisy.result-trust/v1", RunnerID: identity.RunnerID, TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Keys: []resultreport.ResultTrustKey{{KeyID: "result-key", PublicKey: base64.RawURLEncoding.EncodeToString(public), Status: "active", NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}}}
			path := filepath.Join(t.TempDir(), "trust.json")
			save := func() {
				raw, _ := json.Marshal(trust)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			save()
			server := &RunnerServer{Store: memory, Audit: memory, ResultVerifier: &resultreport.Verifier{Keys: resultreport.FileKeys{Path: path, Identity: identity}}, Auth: StaticRunnerAuthenticator{"runner-1": {TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup, Token: "auth"}}}
			var body any = resultSubmission{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, DeliveryToken: "delivery", Result: result, Report: report}
			if endpoint == "reconcile" {
				body = recoveryReport{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, RequestHash: hash, Result: result, Report: report}
			}
			raw, _ := json.Marshal(body)
			send := func() int {
				request := httptest.NewRequest(http.MethodPost, "/v1/runner/grants/"+issued.Grant.GrantID+"/"+endpoint, bytes.NewReader(raw))
				request.Header.Set(runnerIDHeader, "runner-1")
				request.Header.Set("Authorization", "Bearer auth")
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				return response.Code
			}
			if status := send(); status != http.StatusNoContent {
				t.Fatalf("active key refused: %d", status)
			}
			before, _ := memory.GetGrantDispatch(ctx, issued.Grant.GrantID)
			trust.Keys[0].Status = "revoked"
			save()
			if status := send(); status != http.StatusForbidden {
				t.Fatalf("revoked key accepted: %d", status)
			}
			after, _ := memory.GetGrantDispatch(ctx, issued.Grant.GrantID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("revoked report changed durable state")
			}
		})
	}
}

type failedReportSigner struct{}

func (failedReportSigner) Sign(context.Context, []byte) (protocol.Signature, error) {
	return protocol.Signature{}, fmt.Errorf("signer unavailable")
}
func TestClientNeverSubmitsUnsignedFallback(t *testing.T) {
	for _, endpoint := range []string{"result", "reconcile"} {
		for _, failure := range []string{"nil", "error"} {
			t.Run(endpoint+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				now := time.Now().UTC()
				memory := store.NewMemory()
				run := approvedTransportRun(now)
				memory.CreateRun(run)
				signer, _, _ := grant.NewDevelopmentSigner("grant")
				issuer, _ := application.NewGrants(memory, signer, "https://control.example", time.Minute)
				issued, _, err := issuer.Issue(ctx, application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
				if err != nil {
					t.Fatal(err)
				}
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(http.StatusNoContent) }))
				defer server.Close()
				client := &RunnerClient{BaseURL: server.URL, RunnerID: "runner-1", TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup}
				if failure == "error" {
					client.ResultSigner = &resultreport.Signer{Signer: failedReportSigner{}, KeyID: "key"}
				}
				result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: issued.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultRejected, ReasonCode: "LOCAL_REFUSAL", CompletedAt: now}
				if endpoint == "result" {
					err = client.submitResult(ctx, Delivery{Grant: issued.Grant, DeliveryToken: "delivery"}, result)
				} else {
					hash, _ := protocol.GrantHash(issued.Grant)
					err = client.ReportRecovery(ctx, store.RunnerActionRecord{GrantID: issued.Grant.GrantID, RequestHash: hash, Result: result})
				}
				if err == nil || requests != 0 {
					t.Fatalf("unsigned fallback: error=%v requests=%d", err, requests)
				}
			})
		}
	}
}
