package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/policy"
	"themisy/internal/store"
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
