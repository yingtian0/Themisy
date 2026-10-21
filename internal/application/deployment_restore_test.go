package application

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	workflowcore "themisy/internal/workflow"
	"themisy/pkg/protocol"
)

func TestRestoredDeploymentIssuesBoundRollbackGrant(t *testing.T) {
	now := time.Now().UTC()
	memory := store.NewMemory()
	run := grantRun(now)
	// This test concerns persistence/Grant binding, not the production runtime gate.
	run.Environment = domain.EnvironmentStaging
	run.Plan.Environment = domain.EnvironmentStaging
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	signer, _, err := grant.NewDevelopmentSigner("test-key")
	if err != nil {
		t.Fatal(err)
	}
	grants, err := NewGrants(memory, signer, "https://control.example", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	grants.Now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observed := make(chan protocol.ActionGrant, 2)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 2; i++ {
			var record store.GrantDispatchRecord
			for {
				var err error
				record, err = memory.ClaimGrantDispatch(ctx, run.TenantID, "payments-prod", "runner-1", fmt.Sprintf("delivery-%d", i), now, now.Add(time.Minute))
				if err == nil {
					break
				}
				if !errors.Is(err, store.ErrNotFound) {
					done <- err
					return
				}
				select {
				case <-ctx.Done():
					done <- ctx.Err()
					return
				case <-time.After(time.Millisecond):
				}
			}
			observed <- record.Grant
			if _, err := memory.AcknowledgeGrantDispatch(ctx, record.Grant.GrantID, "runner-1", record.DeliveryToken, now, now.Add(time.Minute)); err != nil {
				done <- err
				return
			}
			result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: record.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: fmt.Sprintf("ecs:%d", i), CompletedAt: now.Add(time.Second)}
			audit := testGrantAudit(now)
			audit.ID = fmt.Sprintf("result-%d", i)
			if _, err := memory.CompleteGrantDispatch(ctx, record.Grant.GrantID, "runner-1", record.DeliveryToken, result, now.Add(time.Second), audit); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	exec := &GrantExecutor{Grants: grants, PollInterval: time.Millisecond}
	input := workflowcore.DeployInput{RunID: run.ID, Service: "payment-api", Environment: "staging", DesiredVersion: run.Steps[1].Change.DesiredVersion, IdempotencyKey: "restore-grant"}
	first, err := workflowcore.NewActivities(memory, policy.New(), exec).Deploy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	// A replacement worker receives only the durable result, never a cached Deployment.
	restored, err := workflowcore.NewActivities(memory, policy.New(), executor.NewMock(nil)).Deploy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if restored != first {
		t.Fatalf("restore differs: %+v %+v", restored, first)
	}
	current, err := memory.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Steps[1].Status = domain.StepRollingBack
	current.Steps[1].Execution = &restored.Execution
	if err := memory.UpdateRun(current, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	rollback, err := exec.Rollback(ctx, restored.Deployment)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deployGrant, rollbackGrant := <-observed, <-observed
	if rollbackGrant.RunID != run.ID || rollbackGrant.StepID != "payment-api" || rollbackGrant.Target != deployGrant.Target || rollbackGrant.Action.Capability != protocol.CapabilityRollback || rollbackGrant.Action.ExternalExecutionID != first.Deployment.ExternalID || rollbackGrant.IdempotencyKey != input.IdempotencyKey+"/rollback" || rollback.IdempotencyKey != rollbackGrant.IdempotencyKey {
		t.Fatalf("incorrect rollback binding: %+v", rollbackGrant)
	}
}
