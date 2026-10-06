package transport

import (
	"context"
	"net/http/httptest"
	"testing"
	"themisy/internal/application"
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
