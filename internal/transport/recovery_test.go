package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

func TestRecoveryReportsAfterExpiryAndRejectsConflicts(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	memory := store.NewMemory()
	run := approvedTransportRun(now)
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	signer, _, err := grant.NewDevelopmentSigner("key")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := application.NewGrants(memory, signer, "https://control.example", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issued, _, err := issuer.Issue(ctx, application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
	if err != nil {
		t.Fatal(err)
	}
	// Recovery must not require a still-valid delivery token or Grant lifetime.
	auth := StaticRunnerAuthenticator{"replacement": {TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup, Token: "secret"}, "other": {TenantID: "other", RunnerGroup: issued.Grant.RunnerGroup, Token: "secret"}}
	server := httptest.NewServer((&RunnerServer{Store: memory, Audit: memory, Auth: auth, Now: func() time.Time { return now.Add(time.Hour) }}).Handler())
	defer server.Close()
	client := &RunnerClient{BaseURL: server.URL, RunnerID: "replacement", Token: "secret"}
	hash, _ := protocol.GrantHash(issued.Grant)
	record := store.RunnerActionRecord{GrantID: issued.Grant.GrantID, RequestHash: hash, Result: protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: issued.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultUnknown, CompletedAt: now}}
	if err := client.ReportRecovery(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.Result.Status = protocol.ResultSucceeded
	record.Result.ExternalExecutionID = "observed-ecs-deployment"
	if err := client.ReportRecovery(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportRecovery(ctx, record); err != nil {
		t.Fatalf("idempotent report: %v", err)
	}
	loaded, err := memory.GetGrantDispatch(ctx, record.GrantID)
	if err != nil || loaded.Status != store.GrantDispatchSucceeded {
		t.Fatalf("record=%#v err=%v", loaded, err)
	}
	record.RequestHash = "wrong"
	if err := client.ReportRecovery(ctx, record); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	record.RequestHash = hash
	record.Result.ExternalExecutionID = "different"
	if err := client.ReportRecovery(ctx, record); err == nil {
		t.Fatal("terminal mismatch accepted")
	}
	client.RunnerID = "other"
	if err := client.ReportRecovery(ctx, record); err == nil {
		t.Fatal("cross tenant report accepted")
	}
	events, _ := memory.AuditEvents(run.ID)
	conflicts := 0
	for _, event := range events {
		if event.Action == "grant.reconcile" && event.Result == "CONFLICT" {
			conflicts++
		}
	}
	if conflicts != 2 {
		t.Fatalf("conflict audits=%d", conflicts)
	}
}

// Recovery is a reporting-only endpoint: RunnerServer has no credential broker
// or provider adapter. These requests cannot invoke a provider write.
func TestRecoveryConflictAuditPersistence(t *testing.T) {
	for _, conflict := range []string{"hash_mismatch", "terminal_result_mismatch"} {
		for _, unavailable := range []bool{false, true} {
			name := conflict + "/audit_available"
			if unavailable {
				name = conflict + "/audit_unavailable"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				now := time.Now().UTC()
				memory := store.NewMemory()
				run := approvedTransportRun(now)
				if _, _, err := memory.CreateRun(run); err != nil {
					t.Fatal(err)
				}
				signer, _, err := grant.NewDevelopmentSigner("key")
				if err != nil {
					t.Fatal(err)
				}
				issuer, err := application.NewGrants(memory, signer, "https://control.example", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				issued, _, err := issuer.Issue(ctx, application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
				if err != nil {
					t.Fatal(err)
				}
				hash, err := protocol.GrantHash(issued.Grant)
				if err != nil {
					t.Fatal(err)
				}
				result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: issued.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "original-deployment", CompletedAt: now}
				if err := memory.ReconcileGrantDispatch(ctx, issued.Grant.GrantID, issued.StateVersion, result, domain.AuditEvent{ID: "original-recovery", CorrelationID: run.ID, Timestamp: now}); err != nil {
					t.Fatal(err)
				}
				before, err := memory.GetGrantDispatch(ctx, issued.Grant.GrantID)
				if err != nil {
					t.Fatal(err)
				}
				body := recoveryReport{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, RequestHash: hash, Result: result}
				if conflict == "hash_mismatch" {
					body.RequestHash = "wrong"
				} else {
					body.Result.ExternalExecutionID = "conflicting-deployment"
				}
				audit := &recoveryAuditSink{memory: memory, unavailable: unavailable}
				server := &RunnerServer{Store: memory, Audit: audit, Auth: StaticRunnerAuthenticator{"replacement": {TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup, Token: "secret"}}, Now: func() time.Time { return now.Add(time.Hour) }}
				payload, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/runner/grants/"+issued.Grant.GrantID+"/reconcile", bytes.NewReader(payload))
				request.Header.Set(runnerIDHeader, "replacement")
				request.Header.Set("Authorization", "Bearer secret")
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				wantStatus := http.StatusConflict
				if unavailable {
					wantStatus = http.StatusServiceUnavailable
				}
				if response.Code != wantStatus {
					t.Errorf("status=%d want=%d body=%s", response.Code, wantStatus, response.Body.String())
				}
				if len(audit.attempts) != 1 || audit.attempts[0].Action != "grant.reconcile" || audit.attempts[0].Result != "CONFLICT" || audit.attempts[0].ResourceID != issued.Grant.GrantID {
					t.Errorf("conflict audit attempts=%#v", audit.attempts)
				}
				after, err := memory.GetGrantDispatch(ctx, issued.Grant.GrantID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Errorf("conflict changed durable dispatch: before=%#v after=%#v", before, after)
				}
				events, err := memory.AuditEvents(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				conflicts := 0
				for _, event := range events {
					if event.Action == "grant.reconcile" && event.Result == "CONFLICT" {
						conflicts++
					}
				}
				wantConflicts := 1
				if unavailable {
					wantConflicts = 0
				}
				if conflicts != wantConflicts {
					t.Errorf("persisted conflicts=%d want=%d", conflicts, wantConflicts)
				}
			})
		}
	}
}

type recoveryAuditSink struct {
	memory      *store.Memory
	unavailable bool
	attempts    []domain.AuditEvent
}

func (s *recoveryAuditSink) AppendAudit(event domain.AuditEvent) error {
	s.attempts = append(s.attempts, event)
	if s.unavailable {
		return errors.New("audit unavailable")
	}
	return s.memory.AppendAudit(event)
}
