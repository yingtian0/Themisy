package application

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

func TestCompensationAdmission(t *testing.T) { testCompensationAdmission(t, nil) }
func testCompensationAdmission(t *testing.T, shared store.DurableStore) {
	for _, scenario := range []string{"valid", "deadline", "unknown-source", "wrong-source", "unknown-target", "revoked-approval", "expired-approval", "expired-plan"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			original := now
			var st store.DurableStore = store.NewMemory()
			if shared != nil {
				st = shared
			}
			run := grantRun(now)
			run.ID = fmt.Sprintf("compensation-%d", time.Now().UnixNano())
			run.TenantID = run.ID
			run.RequestID = run.ID
			run.WorkflowID = run.ID
			run.Plan.CreatedAt = now.Add(-time.Minute)
			run.Plan.ExpiresAt = now.Add(15 * time.Minute)
			run.Plan.Phases[0].Steps[1].RollbackDeadline = "2m"
			run.Plan.Phases[0].Steps[1].RollbackTargetHash = "sha256:" + strings.Repeat("b", 64)
			run.Steps[1].Approval.ExpiresAt = run.Plan.ExpiresAt
			if _, _, err := st.CreateRun(run); err != nil {
				t.Fatal(err)
			}
			signer, _, _ := grant.NewDevelopmentSigner("grant-key")
			issuer, err := NewGrants(st, signer, "control", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			issuer.Now = func() time.Time { return now }
			forward, _, err := issuer.Issue(ctx, GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := st.ClaimGrantDispatch(ctx, run.TenantID, "payments-prod", "runner", "delivery", now, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AcknowledgeGrantDispatch(ctx, forward.Grant.GrantID, "runner", claimed.DeliveryToken, now, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.CompleteGrantDispatch(ctx, forward.Grant.GrantID, "runner", claimed.DeliveryToken, protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: forward.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "ecs:source", CompletedAt: now}, now, domain.AuditEvent{ID: run.ID + "-complete", CorrelationID: run.ID, Action: "test.source", Timestamp: now}); err != nil {
				t.Fatal(err)
			}
			run, err = st.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			run.Steps[1].Status = domain.StepRollingBack
			run.Steps[1].Execution = &domain.Execution{ID: "execution", Status: string(store.ExecutionSucceeded), ExternalExecutionID: "ecs:source", IdempotencyKey: forward.Grant.IdempotencyKey, StartedAt: now, FinishedAt: now}
			request := GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityRollback, SourceGrantID: forward.Grant.GrantID, ExternalExecutionID: "ecs:source"}
			switch scenario {
			case "deadline":
				now = now.Add(2 * time.Minute)
			case "unknown-source":
				run.Steps[1].Execution.Status = string(store.ExecutionUnknown)
			case "wrong-source":
				request.ExternalExecutionID = "ecs:unrelated"
			case "unknown-target":
				run.Plan.Phases[0].Steps[1].RollbackTargetHash = ""
			case "revoked-approval":
				run.Steps[1].Approval.Status = domain.ApprovalRevoked
			case "expired-approval":
				run.Steps[1].Approval.ExpiresAt = now
			case "expired-plan":
				now = run.Plan.ExpiresAt
			}
			if err := st.UpdateRun(run, run.StateVersion); err != nil {
				t.Fatal(err)
			}
			approvalSigner, _, _ := grant.NewDevelopmentSigner("independent-approval")
			manifest, mErr := ApprovedManifest(ctx, st, approvalSigner, "approval", request, 1, now)
			rollback, fresh, err := issuer.Issue(ctx, request)
			if scenario != "valid" {
				if err == nil || fresh || mErr == nil {
					t.Fatalf("unsafe compensation admitted: grant=%v fresh=%v manifest=%v", err, fresh, mErr)
				}
				return
			}
			if err != nil || !fresh || mErr != nil {
				t.Fatalf("grant=%v manifest=%v", err, mErr)
			}
			if !protocol.SameCompensation(rollback.Grant.Compensation, manifest.Compensation) || rollback.Grant.Compensation.SourceGrantID != forward.Grant.GrantID || rollback.Grant.AuthorizationEpoch != manifest.AuthorizationEpoch || !rollback.Grant.ExpiresAt.Equal(now.Add(time.Minute)) {
				t.Fatal("independent manifest/short Grant disagreed")
			}
			until, _ := time.Parse(time.RFC3339Nano, rollback.Grant.Compensation.ValidUntil)
			if !until.Equal(original.Add(2 * time.Minute)) {
				t.Fatal("compensation deadline restarted at response/rollback")
			}
		})
	}
}
