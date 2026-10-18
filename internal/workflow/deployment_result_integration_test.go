//go:build integration

package workflow

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/policy"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/protocol"
)

// Hide the scheduler extension for this Activity-level test; the tested
// boundary is durable execution and Grant recovery, not schedule admission.
type durableStoreNoCoordinator struct{ store.DurableStore }

func TestPostgresDeploymentSuccessResponseLoss(t *testing.T) {
	url := os.Getenv("THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL")
	if url == "" && os.Getenv("THEMISY_INTEGRATION") == "1" {
		url = os.Getenv("THEMISY_DATABASE_URL")
		if url == "" {
			url = "postgres://themisy:themisy@localhost:5432/themisy?sslmode=disable"
		}
	}
	if url == "" {
		t.Skip("requires dedicated disposable THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL")
	}
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	st, err := postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	run, _, exec := workflowFixture(t, false, nil)
	run.ID += "-" + time.Now().UTC().Format("20060102150405.000000000")
	run.RequestID, run.WorkflowID = run.ID, run.ID
	if _, _, err := st.CreateRun(&run); err != nil {
		t.Fatal(err)
	}
	input := DeployInput{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: run.ID + "/pg-response-loss"}
	counted := &countRollbackExecutor{ReleaseExecutor: exec}
	a := NewActivities(lostSuccessResponseStore{st}, policy.New(), counted)
	if _, err := a.Deploy(context.Background(), input); err == nil {
		t.Fatal("expected lost success response")
	}
	// Drop all DB connections and reassemble Activities as a new worker would.
	st.Close()
	st, err = postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	noWrites := &forbidWriteExecutor{ReleaseExecutor: exec}
	restarted := NewActivities(st, policy.New(), noWrites)
	deploy, err := restarted.Deploy(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if deploy.Deployment.RunID != run.ID || deploy.Deployment.Service != "identity" || deploy.Deployment.Environment != "staging" || deploy.Deployment.DesiredVersion != "sha-1" || deploy.Deployment.IdempotencyKey != input.IdempotencyKey {
		t.Fatalf("incomplete result: %+v", deploy)
	}
	if _, err := restarted.Verify(context.Background(), VerifyInput{Deployment: deploy.Deployment}); err != nil {
		t.Fatal(err)
	}
	a = NewActivities(lostSuccessResponseStore{st}, policy.New(), counted)
	rollbackInput := RollbackInput{Deploy: input, Deployment: deploy.Deployment}
	if _, err := a.Rollback(context.Background(), rollbackInput); err == nil {
		t.Fatal("expected lost rollback response")
	}
	st.Close()
	st, err = postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	restarted = NewActivities(st, policy.New(), noWrites)
	rollback, err := restarted.Rollback(context.Background(), rollbackInput)
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.Rollback(context.Background(), rollbackInput)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rollback, again) {
		t.Fatal("rollback result changed after reload")
	}
	if _, err := restarted.Verify(context.Background(), VerifyInput{Deployment: rollback.Deployment}); err != nil {
		t.Fatal(err)
	}
	if noWrites.calls != 0 || exec.DeployCalls(input.IdempotencyKey) != 1 || counted.rollbacks != 1 {
		t.Fatal("duplicate write")
	}
}

func TestPostgresUnknownGrantRecoveryAfterWorkerReplacement(t *testing.T) {
	url := os.Getenv("THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires dedicated disposable THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL")
	}
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	st, err := postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, _, exec := workflowFixture(t, false, nil)
	run.ID += "-unknown-" + time.Now().UTC().Format("20060102150405.000000000")
	run.RequestID, run.WorkflowID = run.ID, run.ID
	run.TenantID = "tenant-test"
	run.Plan.Phases[0].Steps[0].Scheduling = domain.SchedulingContext{TenantID: run.TenantID, Environment: run.Environment, RunnerGroup: "runner-group"}
	if _, _, err := st.CreateRun(&run); err != nil {
		t.Fatal(err)
	}
	input := DeployInput{RunID: run.ID, RequestedBy: run.RequestedBy, AgentID: run.Agent.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: run.ID + "/deployment"}
	grantID := run.ID + "-grant"
	a := NewActivities(durableStoreNoCoordinator{DurableStore: st}, policy.New(), pendingGrantExecutor{ReleaseExecutor: exec, grantID: grantID})
	if _, err := a.Deploy(context.Background(), input); err == nil {
		t.Fatal("pending Grant must remain unresolved")
	}
	unknownExecution, err := st.GetExecution(a.AdapterName, input.IdempotencyKey)
	if err != nil || unknownExecution.Status != store.ExecutionUnknown || unknownExecution.Payload["grant_id"] != grantID {
		t.Fatalf("execution=%+v err=%v", unknownExecution, err)
	}
	run.Status = domain.RunBlocked
	run.Steps[0].Status = domain.StepUnknown
	if err := st.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
		t.Fatal(err)
	}
	grant := protocol.ActionGrant{GrantID: grantID, RunID: run.ID, StepID: input.Service, TenantID: run.TenantID, RunnerGroup: "runner-group", Target: protocol.Target{Service: input.Service, Environment: input.Environment}, Action: protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: input.DesiredVersion}, PlanHash: run.Plan.Hash, IdempotencyKey: input.IdempotencyKey, IssuedAt: run.CreatedAt, ExpiresAt: run.CreatedAt.Add(time.Hour)}
	dispatch, created, err := st.CreateGrantDispatch(context.Background(), store.GrantDispatchRecord{Grant: grant}, domain.AuditEvent{ID: run.ID + "-grant-issued", CorrelationID: run.ID, Timestamp: run.CreatedAt}, domain.OutboxEvent{ID: run.ID + "-grant-outbox", AggregateID: grantID, CreatedAt: run.CreatedAt, AvailableAt: run.CreatedAt})
	if err != nil || !created {
		t.Fatalf("grant dispatch created=%v err=%v", created, err)
	}
	dispatch, err = st.ClaimGrantDispatch(context.Background(), run.TenantID, grant.RunnerGroup, "runner-1", "delivery-token", run.CreatedAt, run.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err = st.AcknowledgeGrantDispatch(context.Background(), grantID, "runner-1", dispatch.DeliveryToken, run.CreatedAt, run.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	unknownResult := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grantID, RunID: run.ID, StepID: input.Service, Status: protocol.ResultUnknown, ReasonCode: "RECONCILIATION_REQUIRED", CompletedAt: run.CreatedAt.Add(time.Second)}
	dispatch, err = st.CompleteGrantDispatch(context.Background(), grantID, "runner-1", dispatch.DeliveryToken, unknownResult, unknownResult.CompletedAt, domain.AuditEvent{ID: run.ID + "-grant-unknown", CorrelationID: run.ID, Timestamp: unknownResult.CompletedAt})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := a.RecoverExecution(context.Background(), RecoverExecutionInput{RunID: run.ID, Service: input.Service, Environment: input.Environment, DesiredVersion: input.DesiredVersion, IdempotencyKey: input.IdempotencyKey}); err != nil || !result.Supported || result.Ready {
		t.Fatalf("unknown result=%+v err=%v", result, err)
	}
	st.Close()
	st, err = postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	restarted := NewActivities(durableStoreNoCoordinator{DurableStore: st}, policy.New(), &forbidWriteExecutor{ReleaseExecutor: exec})
	recovered := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grantID, RunID: run.ID, StepID: input.Service, Status: protocol.ResultSucceeded, ExternalExecutionID: "deployment-recovered", CompletedAt: run.CreatedAt.Add(2 * time.Second)}
	if err := st.ReconcileGrantDispatch(context.Background(), grantID, dispatch.StateVersion, recovered, domain.AuditEvent{ID: run.ID + "-grant-reconciled", CorrelationID: run.ID, Timestamp: recovered.CompletedAt}); err != nil {
		t.Fatal(err)
	}
	result, err := restarted.RecoverExecution(context.Background(), RecoverExecutionInput{RunID: run.ID, Service: input.Service, Environment: input.Environment, DesiredVersion: input.DesiredVersion, IdempotencyKey: input.IdempotencyKey})
	if err != nil || !result.Ready || result.Deployment.ExternalID != recovered.ExternalExecutionID {
		t.Fatalf("recovered result=%+v err=%v", result, err)
	}
	if _, err := restarted.Deploy(context.Background(), input); err != nil {
		t.Fatal(err)
	}
}
