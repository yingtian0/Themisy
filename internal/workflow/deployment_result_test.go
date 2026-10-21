package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/policy"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

// Commits the success before dropping the persistence response. The Activity
// cannot return its result; the next worker must reload the durable record.
type lostSuccessResponseStore struct{ store.DurableStore }

func TestDeploymentRetryPreservesAllFields(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	input := DeployInput{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: "retry-fields"}
	first, err := NewActivities(memory, policy.New(), exec).Deploy(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewActivities(memory, policy.New(), exec).Deploy(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("retry changed typed result: first=%+v second=%+v", first, second)
	}
}

func (s lostSuccessResponseStore) CompleteExecution(r store.ExecutionRecord, v int64, audit domain.AuditEvent, outbox domain.OutboxEvent) error {
	if err := s.DurableStore.CompleteExecution(r, v, audit, outbox); err != nil {
		return err
	}
	return errors.New("injected response loss after commit")
}

type forbidWriteExecutor struct {
	executor.ReleaseExecutor
	calls int
}

type countRollbackExecutor struct {
	executor.ReleaseExecutor
	rollbacks int
}

type pendingGrantExecutor struct {
	executor.ReleaseExecutor
	grantID string
}

type identifiedGrantError struct{ id string }

func (e identifiedGrantError) Error() string   { return "grant result pending" }
func (e identifiedGrantError) GrantID() string { return e.id }

func (e pendingGrantExecutor) Deploy(context.Context, executor.DeployRequest) (executor.Deployment, error) {
	return executor.Deployment{}, identifiedGrantError{id: e.grantID}
}

func (e *countRollbackExecutor) Rollback(ctx context.Context, d executor.Deployment) (executor.Deployment, error) {
	e.rollbacks++
	return e.ReleaseExecutor.Rollback(ctx, d)
}

func (e *forbidWriteExecutor) Deploy(context.Context, executor.DeployRequest) (executor.Deployment, error) {
	e.calls++
	return executor.Deployment{}, errors.New("unexpected deploy")
}
func (e *forbidWriteExecutor) Rollback(context.Context, executor.Deployment) (executor.Deployment, error) {
	e.calls++
	return executor.Deployment{}, errors.New("unexpected rollback")
}

func TestSuccessResponseLossRestoresCompleteDeployment(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	input := DeployInput{RunID: run.ID, RequestedBy: run.RequestedBy, AgentID: run.Agent.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: "restored"}
	counted := &countRollbackExecutor{ReleaseExecutor: exec}
	a := NewActivities(lostSuccessResponseStore{memory}, policy.New(), counted)
	if _, err := a.Deploy(context.Background(), input); err == nil {
		t.Fatal("expected lost response")
	}
	row, err := memory.GetExecution(a.AdapterName, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	var saved deploymentResult
	if err := json.Unmarshal([]byte(row.Payload["deployment_result"].(string)), &saved); err != nil {
		t.Fatal(err)
	}
	noWrites := &forbidWriteExecutor{ReleaseExecutor: exec}
	restarted := NewActivities(memory, policy.New(), noWrites)
	restored, err := restarted.Deploy(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Deployment, saved.Deployment) || restored.Execution.ID != row.ID {
		t.Fatalf("restored=%+v saved=%+v", restored, saved)
	}
	if _, err := restarted.Verify(context.Background(), VerifyInput{Deployment: restored.Deployment}); err != nil {
		t.Fatal(err)
	}
	// Rollback consumes the recovered source; persist its success then lose that response too.
	if _, err := a.Rollback(context.Background(), RollbackInput{Deploy: input, Deployment: restored.Deployment}); err == nil {
		t.Fatal("expected lost rollback response")
	}
	rollback, err := restarted.Rollback(context.Background(), RollbackInput{Deploy: input, Deployment: restored.Deployment})
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Deployment.RunID != run.ID || rollback.Deployment.Service != "identity" || rollback.Deployment.IdempotencyKey != input.IdempotencyKey+"/rollback" || rollback.Execution.ExternalExecutionID != rollback.Deployment.ExternalID {
		t.Fatalf("rollback=%+v", rollback)
	}
	if _, err := restarted.Verify(context.Background(), VerifyInput{Deployment: rollback.Deployment}); err != nil {
		t.Fatal(err)
	}
	if noWrites.calls != 0 || exec.DeployCalls(input.IdempotencyKey) != 1 || counted.rollbacks != 1 {
		t.Fatalf("unexpected writes: %d", noWrites.calls)
	}
}

func TestUnknownGrantRecoveryPersistsAndResolvesSameGrantWithoutWrite(t *testing.T) {
	run, memory, exec := workflowFixture(t, false, nil)
	run.TenantID = "tenant-test"
	run.Plan.Phases[0].Steps[0].Scheduling = domain.SchedulingContext{TenantID: run.TenantID, Environment: run.Environment, RunnerGroup: "runner-group"}
	if err := memory.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
		t.Fatal(err)
	}
	input := DeployInput{RunID: run.ID, RequestedBy: run.RequestedBy, AgentID: run.Agent.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: "grant-recovery"}
	grantID := "grant-recovery-1"
	a := NewActivities(memory, policy.New(), pendingGrantExecutor{ReleaseExecutor: exec, grantID: grantID})
	if _, err := a.Deploy(context.Background(), input); err == nil {
		t.Fatal("pending grant must not be reported as a deployment success")
	}
	unknown, err := memory.GetExecution(a.AdapterName, input.IdempotencyKey)
	if err != nil || unknown.Status != store.ExecutionUnknown || unknown.Payload["grant_id"] != grantID {
		t.Fatalf("unknown execution=%+v err=%v", unknown, err)
	}
	run.Status = domain.RunBlocked
	run.Steps[0].Status = domain.StepUnknown
	if err := memory.UpdateRunAtomic(&run, run.StateVersion, nil, nil); err != nil {
		t.Fatal(err)
	}
	grant := protocol.ActionGrant{GrantID: grantID, RunID: run.ID, StepID: input.Service, TenantID: run.TenantID, RunnerGroup: "runner-group", Target: protocol.Target{Service: input.Service, Environment: input.Environment}, Action: protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: input.DesiredVersion}, PlanHash: run.Plan.Hash, IdempotencyKey: input.IdempotencyKey, IssuedAt: run.CreatedAt, ExpiresAt: run.CreatedAt.Add(time.Hour)}
	if _, _, err := memory.CreateGrantDispatch(context.Background(), store.GrantDispatchRecord{Grant: grant}, domain.AuditEvent{ID: "grant-issue"}, domain.OutboxEvent{ID: "grant-outbox"}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := memory.ClaimGrantDispatch(context.Background(), run.TenantID, "runner-group", "runner-1", "delivery-token", run.CreatedAt, run.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err = memory.AcknowledgeGrantDispatch(context.Background(), grantID, "runner-1", dispatch.DeliveryToken, run.CreatedAt, run.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	unknownResult := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grantID, RunID: run.ID, StepID: input.Service, Status: protocol.ResultUnknown, ReasonCode: "RECONCILIATION_REQUIRED", CompletedAt: run.CreatedAt.Add(time.Second)}
	dispatch, err = memory.CompleteGrantDispatch(context.Background(), grantID, "runner-1", dispatch.DeliveryToken, unknownResult, unknownResult.CompletedAt, domain.AuditEvent{ID: "grant-unknown"})
	if err != nil {
		t.Fatal(err)
	}
	recoveryInput := RecoverExecutionInput{RunID: run.ID, Service: input.Service, Environment: input.Environment, DesiredVersion: input.DesiredVersion, IdempotencyKey: input.IdempotencyKey}
	if result, err := a.RecoverExecution(context.Background(), recoveryInput); err != nil || !result.Supported || result.Ready {
		t.Fatalf("unresolved recovery=%+v err=%v", result, err)
	}
	recovered := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grantID, RunID: run.ID, StepID: input.Service, Status: protocol.ResultSucceeded, ExternalExecutionID: "deployment-recovered", CompletedAt: run.CreatedAt.Add(2 * time.Second)}
	if err := memory.ReconcileGrantDispatch(context.Background(), grantID, dispatch.StateVersion, recovered, domain.AuditEvent{ID: "grant-reconciled", Timestamp: recovered.CompletedAt}); err != nil {
		t.Fatal(err)
	}
	result, err := a.RecoverExecution(context.Background(), recoveryInput)
	if err != nil || !result.Ready || result.Failed || result.Deployment.ExternalID != recovered.ExternalExecutionID {
		t.Fatalf("recovered=%+v err=%v", result, err)
	}
	if calls := exec.DeployCalls(input.IdempotencyKey); calls != 0 {
		t.Fatalf("recovery performed a provider write: calls=%d", calls)
	}
}

func TestRestoreRejectsLegacyUnknownAndBrokenBindings(t *testing.T) {
	for _, mutate := range []string{"legacy", "unknown", "run", "service", "version", "execution", "desired", "external", "timestamp"} {
		t.Run(mutate, func(t *testing.T) {
			run, memory, exec := workflowFixture(t, false, nil)
			input := DeployInput{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: "key"}
			a := NewActivities(memory, policy.New(), exec)
			first, err := a.Deploy(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			row, _ := memory.GetExecution(a.AdapterName, input.IdempotencyKey)
			var saved deploymentResult
			if err := json.Unmarshal([]byte(row.Payload["deployment_result"].(string)), &saved); err != nil {
				t.Fatal(err)
			}
			switch mutate {
			case "legacy":
				row.Payload = map[string]any{"started_at": first.Deployment.StartedAt.String()}
			case "unknown":
				row.Status = store.ExecutionUnknown
			case "run":
				input.RunID = "another"
				other := run
				other.ID = "another"
				other.RequestID = "another"
				if _, _, err := memory.CreateRun(&other); err != nil {
					t.Fatal(err)
				}
			case "service":
				input.Service = "another"
			case "version":
				saved.Version = 99
			case "execution":
				saved.ExecutionID = "another"
			case "desired":
				input.DesiredVersion = "another"
			case "external":
				saved.Deployment.ExternalID = "another"
			case "timestamp":
				saved.Deployment.FinishedAt = saved.Deployment.StartedAt.Add(-1)
			}
			if mutate != "legacy" {
				data, _ := json.Marshal(saved)
				row.Payload = map[string]any{"deployment_result": string(data)}
			}
			if err := memory.CompleteExecution(row, row.StateVersion, domain.AuditEvent{ID: "mutated"}, domain.OutboxEvent{ID: "mutated"}); err != nil {
				t.Fatal(err)
			}
			noWrites := &forbidWriteExecutor{ReleaseExecutor: exec}
			a = NewActivities(memory, policy.New(), noWrites)
			if _, err := a.Deploy(context.Background(), input); err == nil {
				t.Fatal("invalid result returned as success")
			}
			if noWrites.calls != 0 {
				t.Fatal("retry performed a write")
			}
		})
	}
}

func TestRollbackRejectsAlteredOrLegacySourceWithoutWrite(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		run, memory, exec := workflowFixture(t, false, nil)
		input := DeployInput{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: "source"}
		a := NewActivities(memory, policy.New(), exec)
		deploy, err := a.Deploy(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		if legacy {
			row, _ := memory.GetExecution(a.AdapterName, input.IdempotencyKey)
			row.Payload = map[string]any{"started_at": deploy.Deployment.StartedAt.String()}
			if err := memory.CompleteExecution(row, row.StateVersion, domain.AuditEvent{ID: "legacy"}, domain.OutboxEvent{ID: "legacy"}); err != nil {
				t.Fatal(err)
			}
		} else {
			deploy.Deployment.ExternalID = "different-operation"
		}
		noWrites := &forbidWriteExecutor{ReleaseExecutor: exec}
		if _, err := NewActivities(memory, policy.New(), noWrites).Rollback(context.Background(), RollbackInput{Deploy: input, Deployment: deploy.Deployment}); err == nil {
			t.Fatal("invalid rollback source accepted")
		}
		if noWrites.calls != 0 {
			t.Fatal("invalid rollback source performed Write")
		}
	}
}
