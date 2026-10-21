package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

func TestGrantIssueRequiresAuthorizedStepAndPersistsDispatch(t *testing.T) {
	now := time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)
	memory := store.NewMemory()
	run := grantRun(now)
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	signer, publicKey, err := grant.NewDevelopmentSigner("dev-grant-key")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewGrants(memory, signer, "https://control.example", fiveMinutes)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now }
	record, created, err := service.Issue(context.Background(), GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
	if err != nil || !created {
		t.Fatalf("issue created=%v err=%v", created, err)
	}
	if record.Grant.Target.Service != "payment-api" || record.Grant.Target.Environment != "production" {
		t.Fatalf("grant target=%#v", record.Grant.Target)
	}
	if strings.Contains(string(mustGrantJSON(t, record.Grant)), "arn:aws:ecs") {
		t.Fatal("grant leaked a provider resource identifier")
	}
	verifier := grant.Verifier{Issuer: record.Grant.Issuer, RunnerGroup: "payments-prod", TenantID: "tenant-1", Keys: grant.StaticKeys{record.Grant.Issuer + "\x00dev-grant-key": publicKey}, Now: func() time.Time { return now }}
	if err := verifier.Verify(context.Background(), record.Grant); err != nil {
		t.Fatalf("verify signed grant: %v", err)
	}
	pending, err := memory.PendingOutboxByType("grant.dispatch.requested", 10)
	if err != nil || len(pending) != 1 || pending[0].AggregateID != record.Grant.GrantID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	duplicate, created, err := service.Issue(context.Background(), GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
	if err != nil || created || duplicate.Grant.GrantID != record.Grant.GrantID {
		t.Fatalf("duplicate=%#v created=%v err=%v", duplicate, created, err)
	}
}

func TestGrantExecutorWaitsForRunnerResult(t *testing.T) {
	now := time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)
	memory := store.NewMemory()
	run := grantRun(now)
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	signer, _, _ := grant.NewDevelopmentSigner("key")
	service, _ := NewGrants(memory, signer, "https://control.example", fiveMinutes)
	service.Now = func() time.Time { return now }
	done := make(chan error, 1)
	go func() {
		for range 100 {
			record, err := memory.ClaimGrantDispatch(context.Background(), run.TenantID, "payments-prod", "runner-1", "delivery-token", now, now.Add(time.Minute))
			if errors.Is(err, store.ErrNotFound) {
				time.Sleep(time.Millisecond)
				continue
			}
			if err != nil {
				done <- err
				return
			}
			_, err = memory.AcknowledgeGrantDispatch(context.Background(), record.Grant.GrantID, "runner-1", record.DeliveryToken, now, now.Add(time.Minute))
			if err == nil {
				unknown := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: record.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultUnknown, ReasonCode: "RECONCILIATION_REQUIRED", CompletedAt: now.Add(time.Second)}
				var unresolved store.GrantDispatchRecord
				unresolved, err = memory.CompleteGrantDispatch(context.Background(), record.Grant.GrantID, "runner-1", record.DeliveryToken, unknown, now.Add(time.Second), testGrantAudit(now))
				if err == nil {
					recovered := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: record.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "ecs:deployment", CompletedAt: now.Add(2 * time.Second)}
					err = memory.ReconcileGrantDispatch(context.Background(), record.Grant.GrantID, unresolved.StateVersion, recovered, testGrantAudit(now.Add(2*time.Second)))
				}
			}
			done <- err
			return
		}
		done <- errors.New("grant was not dispatched")
	}()
	execution := &GrantExecutor{Grants: service, PollInterval: time.Millisecond}
	deployment, err := execution.Deploy(context.Background(), executor.DeployRequest{RunID: run.ID, Service: "payment-api", Environment: "production", DesiredVersion: run.Steps[1].Change.DesiredVersion, IdempotencyKey: "run-1/production/payment-api/version"})
	if err != nil || deployment.ExternalID != "ecs:deployment" {
		t.Fatalf("deployment=%#v err=%v", deployment, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGrantExecutorRollbackWaitsForRecoveredRunnerResult(t *testing.T) {
	now := time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)
	memory := store.NewMemory()
	run := grantRun(now)
	run.Steps[1].Status = domain.StepRollingBack
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	signer, _, err := grant.NewDevelopmentSigner("key")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewGrants(memory, signer, "https://control.example", fiveMinutes)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now }
	done := make(chan error, 1)
	go func() {
		for range 100 {
			dispatches, err := memory.PendingOutboxByType("grant.dispatch.requested", 10)
			if err != nil {
				done <- err
				return
			}
			if len(dispatches) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			grantID := dispatches[0].AggregateID
			record, err := memory.ClaimGrantDispatch(context.Background(), run.TenantID, "payments-prod", "runner-1", "rollback-delivery", now, now.Add(time.Minute))
			if err == nil {
				_, err = memory.AcknowledgeGrantDispatch(context.Background(), grantID, "runner-1", record.DeliveryToken, now, now.Add(time.Minute))
			}
			if err == nil {
				unknown := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultUnknown, ReasonCode: "RECONCILIATION_REQUIRED", CompletedAt: now.Add(time.Second)}
				var unresolved store.GrantDispatchRecord
				unresolved, err = memory.CompleteGrantDispatch(context.Background(), grantID, "runner-1", record.DeliveryToken, unknown, now.Add(time.Second), testGrantAudit(now))
				if err == nil {
					recovered := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "ecs:rollback", CompletedAt: now.Add(2 * time.Second)}
					err = memory.ReconcileGrantDispatch(context.Background(), grantID, unresolved.StateVersion, recovered, testGrantAudit(now.Add(2*time.Second)))
				}
			}
			done <- err
			return
		}
		done <- errors.New("rollback grant was not dispatched")
	}()
	execution := &GrantExecutor{Grants: service, PollInterval: time.Millisecond}
	digest := run.Steps[1].Change.DesiredVersion
	deployment, err := execution.Rollback(context.Background(), executor.Deployment{ExternalID: "ecs:deployment", StartedAt: now, FinishedAt: now.Add(time.Second), RunID: run.ID, Service: "payment-api", Environment: "production", DesiredVersion: digest, IdempotencyKey: "run-1/production/payment-api/" + digest})
	if err != nil || deployment.ExternalID != "ecs:rollback" {
		t.Fatalf("rollback deployment=%#v err=%v", deployment, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWaitResultKeepsUnknownPendingUntilExistingGrantRecovers(t *testing.T) {
	now := time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)
	memory := store.NewMemory()
	run := grantRun(now)
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	signer, _, err := grant.NewDevelopmentSigner("key")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewGrants(memory, signer, "https://control.example", fiveMinutes)
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now }
	record, created, err := service.Issue(context.Background(), GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy, IdempotencyKey: "durable-recovery-key"})
	if err != nil || !created {
		t.Fatalf("issue created=%v err=%v", created, err)
	}
	leased, err := memory.ClaimGrantDispatch(context.Background(), run.TenantID, record.Grant.RunnerGroup, "runner-1", "delivery", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	unknown := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: record.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultUnknown, ReasonCode: "RECONCILIATION_REQUIRED", CompletedAt: now.Add(time.Second)}
	unresolved, err := memory.CompleteGrantDispatch(context.Background(), record.Grant.GrantID, "runner-1", leased.DeliveryToken, unknown, now.Add(time.Second), testGrantAudit(now.Add(time.Second)))
	if err != nil || unresolved.Status != store.GrantDispatchUnknown {
		t.Fatalf("unknown status=%s err=%v", unresolved.Status, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resultChannel := make(chan protocol.Result, 1)
	errorChannel := make(chan error, 1)
	go func() {
		result, err := service.WaitResult(ctx, record.Grant.GrantID, time.Millisecond)
		if err != nil {
			errorChannel <- err
			return
		}
		resultChannel <- result
	}()
	time.Sleep(10 * time.Millisecond)
	select {
	case result := <-resultChannel:
		t.Fatalf("UNKNOWN must remain pending, got early result %#v", result)
	case err := <-errorChannel:
		t.Fatalf("UNKNOWN wait ended before recovery: %v", err)
	default:
	}

	// Recovery changes the existing dispatch record; the grant identity/key
	// stay fixed and no second Grant or dispatch is created.
	recovered := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: record.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "ecs:deployment", CompletedAt: now.Add(2 * time.Second)}
	if err := memory.ReconcileGrantDispatch(context.Background(), record.Grant.GrantID, unresolved.StateVersion, recovered, testGrantAudit(now.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-resultChannel:
		if result != recovered {
			t.Fatalf("recovered result=%#v want %#v", result, recovered)
		}
	case err := <-errorChannel:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("WaitResult did not observe recovery on the existing Grant")
	}
	final, err := memory.GetGrantDispatch(context.Background(), record.Grant.GrantID)
	if err != nil || final.StateVersion != unresolved.StateVersion+1 || final.Result != recovered {
		t.Fatalf("final record=%#v err=%v", final, err)
	}
}

func TestGrantIssueFailsClosedWithoutApprovalOrDependency(t *testing.T) {
	now := time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*domain.ReleaseRun)
	}{
		{"approval pending", func(run *domain.ReleaseRun) { run.Steps[1].Approval.Status = domain.ApprovalPending }},
		{"dependency pending", func(run *domain.ReleaseRun) { run.Steps[0].Status = domain.StepPending }},
		{"missing evidence hash", func(run *domain.ReleaseRun) { run.Plan.EvidenceHash = "" }},
		{"step not executing", func(run *domain.ReleaseRun) { run.Steps[1].Status = domain.StepPending }},
	} {
		t.Run(test.name, func(t *testing.T) {
			memory := store.NewMemory()
			run := grantRun(now)
			test.mutate(run)
			if _, _, err := memory.CreateRun(run); err != nil {
				t.Fatal(err)
			}
			signer, _, _ := grant.NewDevelopmentSigner("key")
			service, _ := NewGrants(memory, signer, "https://control.example", fiveMinutes)
			service.Now = func() time.Time { return now }
			_, _, err := service.Issue(context.Background(), GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
			if !errors.Is(err, ErrGrantNotAuthorized) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

const fiveMinutes = 5 * time.Minute

func grantRun(now time.Time) *domain.ReleaseRun {
	engine, err := policy.NewMandatoryEngine(context.Background())
	if err != nil {
		panic(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	decided := now.Add(-time.Minute)
	run := &domain.ReleaseRun{
		ID: "run-1", RequestID: "request-1", WorkflowID: "run-1", ReleaseVersion: "release-1", Environment: domain.EnvironmentProduction,
		RequestedBy: "user-1", SubjectType: "user", SubjectIssuer: "https://idp.example", UserIdentityProof: "signed.oidc.proof", TenantID: "tenant-1", DelegationRef: "delegation-1", Agent: domain.AgentIdentity{ID: "agent-1"},
		Plan:   domain.ReleasePlan{Hash: digest, PlanHash: digest, PolicyHash: digest, EvidenceHash: digest, ExpiresAt: now.Add(time.Hour), Phases: []domain.PlanPhase{{Steps: []domain.PlanStep{{Service: "database", ContractHash: digest, ProfileHash: digest}, {Service: "payment-api", ContractHash: digest, ProfileHash: digest, Dependencies: []domain.Dependency{{Service: "database", Type: domain.DependencyRolloutOrder}}, Scheduling: domain.SchedulingContext{RunnerGroup: "payments-prod", RiskTier: "high"}}}}}},
		Status: domain.RunRunning, StateVersion: 1, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		Steps: []domain.ReleaseStep{{Service: "database", Status: domain.StepSucceeded}, {Service: "payment-api", Status: domain.StepExecuting, Change: domain.Change{Service: "payment-api", DesiredVersion: digest, Risk: "high"}, Policy: &domain.PolicyDecision{Decision: domain.DecisionRequireApproval, InputHash: digest}, Approval: &domain.Approval{ID: "approval-1", Status: domain.ApprovalApproved, PlanHash: digest, ExpiresAt: now.Add(time.Hour), DecidedAt: &decided}}},
	}
	run.Plan.PolicyHash = engine.PolicyHash()
	for index := range run.Steps {
		run.Steps[index].Change.CISuccess = true
		run.Steps[index].Change.DependenciesHealthy = true
	}
	return run
}

func mustGrantJSON(t *testing.T, value protocol.ActionGrant) []byte {
	t.Helper()
	data, err := protocol.EncodeActionGrant(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testGrantAudit(now time.Time) domain.AuditEvent {
	return domain.AuditEvent{ID: "result-audit", CorrelationID: "run-1", ActorType: "runner", ActorID: "runner-1", Action: "grant.result", ResourceType: "grant", ResourceID: "grant-1", Result: "SUCCEEDED", Timestamp: now}
}
