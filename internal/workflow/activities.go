package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/policy"
	"themisy/internal/scheduler"
	"themisy/internal/store"
	verificationcore "themisy/internal/verification"

	"go.temporal.io/sdk/temporal"
)

const (
	ActivityPersistRun       = "PersistRun"
	ActivityEvaluateStep     = "EvaluateStep"
	ActivityDeploy           = "Deploy"
	ActivityVerify           = "Verify"
	ActivityRollback         = "Rollback"
	ActivityAcquireSchedule  = "AcquireSchedule"
	ActivityCompleteSchedule = "CompleteSchedule"
)

type AuditIntent struct {
	ActorType    string
	ActorID      string
	Action       string
	ResourceType string
	ResourceID   string
	Result       string
	Details      map[string]any
}

type PersistInput struct {
	Run       domain.ReleaseRun
	Audits    []AuditIntent
	EventType string
}

type EvaluateInput struct {
	Run  domain.ReleaseRun
	Step domain.ReleaseStep
}

type DeployInput struct {
	RunID          string
	RequestedBy    string
	AgentID        string
	Service        string
	Environment    string
	DesiredVersion string
	IdempotencyKey string
}

type DeployResult struct {
	Execution  domain.Execution
	Deployment executor.Deployment
}

type VerifyInput struct{ Deployment executor.Deployment }

type RollbackInput struct {
	Deploy     DeployInput
	Deployment executor.Deployment
}

type RollbackResult struct {
	Execution  domain.Execution
	Deployment executor.Deployment
}

type ScheduleInput struct {
	RunID string
	Step  scheduler.Step
}

type ScheduleResult struct {
	Allowed   bool
	Reason    string
	BlockedBy []scheduler.Blocked
}

type ScheduleCompleteInput struct {
	RunID  string
	StepID string
	Failed bool
}

type Activities struct {
	Store       store.DurableStore
	Policy      *policy.Engine
	Executor    executor.ReleaseExecutor
	AdapterName string
	Now         func() time.Time
	Scheduler   *scheduler.Scheduler
	Capacity    func(ScheduleInput) scheduler.Capacity
	scheduleMu  sync.Mutex
	scheduled   map[string]scheduler.Dispatch
}

func NewActivities(st store.DurableStore, policyEngine *policy.Engine, releaseExecutor executor.ReleaseExecutor) *Activities {
	coordinator, err := scheduler.New(scheduler.DefaultLimits(), scheduler.CircuitPolicy{MinimumSamples: 5, WindowSize: 20, ErrorRate: .5})
	if err != nil {
		panic(err)
	}
	return &Activities{Store: st, Policy: policyEngine, Executor: releaseExecutor, AdapterName: "mock", Now: time.Now, Scheduler: coordinator, Capacity: func(ScheduleInput) scheduler.Capacity {
		return scheduler.Capacity{RunnerAvailable: 20, AdapterRemaining: 20, QueueLimit: 100}
	}, scheduled: make(map[string]scheduler.Dispatch)}
}

func (a *Activities) AcquireSchedule(_ context.Context, input ScheduleInput) (ScheduleResult, error) {
	key := input.RunID + "\x00" + input.Step.ID
	a.scheduleMu.Lock()
	defer a.scheduleMu.Unlock()
	if _, exists := a.scheduled[key]; exists {
		return ScheduleResult{Allowed: true}, nil
	}
	capacity := a.Capacity(input)
	results := a.Scheduler.Dispatch([]scheduler.Step{input.Step}, capacity)
	if len(results) != 1 {
		return ScheduleResult{}, errors.New("scheduler returned an invalid result count")
	}
	result := results[0]
	if result.Err != nil {
		return ScheduleResult{Reason: result.Err.Error(), BlockedBy: result.BlockedBy}, nil
	}
	if len(result.BlockedBy) > 0 {
		return ScheduleResult{Reason: "concurrency budget exhausted", BlockedBy: result.BlockedBy}, nil
	}
	a.scheduled[key] = result
	return ScheduleResult{Allowed: true}, nil
}

func (a *Activities) CompleteSchedule(_ context.Context, input ScheduleCompleteInput) (scheduler.CircuitState, error) {
	key := input.RunID + "\x00" + input.StepID
	a.scheduleMu.Lock()
	defer a.scheduleMu.Unlock()
	dispatch, exists := a.scheduled[key]
	if !exists {
		return a.Scheduler.CircuitState(""), nil
	}
	delete(a.scheduled, key)
	return a.Scheduler.Complete(dispatch, input.Failed), nil
}

func (a *Activities) PersistRun(_ context.Context, input PersistInput) (domain.ReleaseRun, error) {
	current, err := a.Store.GetRun(input.Run.ID)
	if err != nil {
		return domain.ReleaseRun{}, err
	}
	if current.Environment != input.Run.Environment || current.Plan.Environment != input.Run.Plan.Environment {
		return domain.ReleaseRun{}, verificationGuardError(errors.New("run environment is immutable"))
	}
	if current.Environment == domain.EnvironmentProduction {
		samePlan, err := equivalentPlans(current.Plan, input.Run.Plan)
		if err != nil {
			return domain.ReleaseRun{}, verificationGuardError(err)
		}
		if !samePlan {
			return domain.ReleaseRun{}, verificationGuardError(errors.New("production plan is immutable; regenerate the plan and approval"))
		}
		if len(current.Steps) != len(input.Run.Steps) {
			return domain.ReleaseRun{}, verificationGuardError(errors.New("production verification step mapping is immutable"))
		}
		for i, step := range current.Steps {
			incoming := input.Run.Steps[i]
			if step.Service != incoming.Service || step.Phase != incoming.Phase || step.VerificationRequired != incoming.VerificationRequired {
				return domain.ReleaseRun{}, verificationGuardError(errors.New("production verification step mapping is immutable"))
			}
		}
	}
	successful := input.Run.Status == domain.RunSucceeded
	for _, step := range input.Run.Steps {
		successful = successful || step.Status == domain.StepSucceeded
	}
	if successful {
		for _, run := range []domain.ReleaseRun{*current, input.Run} {
			if err := validateProductionVerification(run); err != nil {
				return domain.ReleaseRun{}, verificationGuardError(err)
			}
		}
	}
	audits := make([]domain.AuditEvent, 0, len(input.Audits))
	for _, intent := range input.Audits {
		audits = append(audits, a.audit(input.Run, intent))
	}
	outbox := []domain.OutboxEvent{}
	if input.EventType != "" {
		outbox = append(outbox, a.outbox("run-event", input.Run.ID, input.EventType, map[string]any{"status": input.Run.Status}))
	}
	expected := input.Run.StateVersion
	if err := a.Store.UpdateRunAtomic(&input.Run, expected, audits, outbox); err != nil {
		if errors.Is(err, store.ErrConflict) {
			current, getErr := a.Store.GetRun(input.Run.ID)
			expectedRun := input.Run
			expectedRun.StateVersion = expected + 1
			if getErr == nil && reflect.DeepEqual(*current, expectedRun) {
				return *current, nil
			}
		}
		return domain.ReleaseRun{}, err
	}
	return input.Run, nil
}

func (a *Activities) EvaluateStep(_ context.Context, input EvaluateInput) (domain.PolicyDecision, error) {
	current, err := a.Store.GetRun(input.Run.ID)
	if err != nil {
		return domain.PolicyDecision{}, err
	}
	for _, run := range []domain.ReleaseRun{*current, input.Run} {
		if err := validateProductionVerification(run); err != nil {
			return domain.PolicyDecision{Decision: domain.DecisionDeny, ReasonCode: "PRODUCTION_VERIFICATION_REQUIRED", ReasonDetail: err.Error(), CreatedAt: a.Now().UTC()}, nil
		}
	}
	if input.Run.Environment != current.Environment || input.Run.Plan.Environment != current.Plan.Environment {
		return domain.PolicyDecision{Decision: domain.DecisionDeny, ReasonCode: "PRODUCTION_VERIFICATION_REQUIRED", ReasonDetail: "run environment is immutable", CreatedAt: a.Now().UTC()}, nil
	}
	decision := a.Policy.Evaluate(policy.InputForRelease(input.Run, input.Step))
	return decision, nil
}

func (a *Activities) Deploy(ctx context.Context, input DeployInput) (DeployResult, error) {
	run, err := a.Store.GetRun(input.RunID)
	if err != nil {
		return DeployResult{}, err
	}
	if err := validateProductionVerification(*run); err != nil {
		return DeployResult{}, verificationGuardError(err)
	}
	if string(run.Environment) != input.Environment {
		return DeployResult{}, verificationGuardError(errors.New("deployment environment differs from durable run"))
	}
	now := a.Now().UTC()
	record := store.ExecutionRecord{ID: newID("execution"), RunID: input.RunID, Service: input.Service, Adapter: a.AdapterName,
		IdempotencyKey: input.IdempotencyKey, CreatedAt: now, UpdatedAt: now, Payload: map[string]any{"desired_version": input.DesiredVersion}}
	audit := a.auditFor(input.RunID, input.RequestedBy, AuditIntent{ActorType: "agent", ActorID: input.AgentID, Action: "deployment.reserve", ResourceType: "service", ResourceID: input.Service, Result: "AUTHORIZED", Details: map[string]any{"adapter": a.AdapterName, "idempotency_key": input.IdempotencyKey}}, now)
	outbox := a.outbox("execution", record.ID, "execution.reserved", map[string]any{"adapter": a.AdapterName, "idempotency_key": input.IdempotencyKey})
	reserved, created, err := a.Store.ReserveExecution(record, audit, outbox)
	if err != nil {
		return DeployResult{}, err
	}
	if !created {
		if reserved.Status == store.ExecutionSucceeded {
			result, err := restoreDeploymentResult(*run, reserved, record)
			if err == nil && result.Deployment.DesiredVersion != input.DesiredVersion {
				return DeployResult{}, resultUnknown(errors.New("retry desired version differs from durable result"))
			}
			return result, err
		}
		return DeployResult{}, temporal.NewNonRetryableApplicationError("reserved execution requires reconciliation", string(ErrorUnknownExternalState), &ClassifiedError{Class: ErrorUnknownExternalState, Operation: "deploy", Err: store.ErrUnknownExternalState})
	}
	deployment, err := a.Executor.Deploy(ctx, executor.DeployRequest{RunID: input.RunID, RequestedBy: input.RequestedBy, AgentID: input.AgentID, Service: input.Service, Environment: input.Environment, DesiredVersion: input.DesiredVersion, IdempotencyKey: input.IdempotencyKey})
	if err != nil {
		reserved.Status = store.ExecutionUnknown
		reserved.UpdatedAt = a.Now().UTC()
		failureAudit := a.auditFor(input.RunID, input.RequestedBy, AuditIntent{ActorType: "system", ActorID: "workflow-worker", Action: "deployment.result", ResourceType: "service", ResourceID: input.Service, Result: "UNKNOWN", Details: map[string]any{"error": err.Error()}}, reserved.UpdatedAt)
		_ = a.Store.CompleteExecution(reserved, reserved.StateVersion, failureAudit, a.outbox("execution", reserved.ID, "execution.unknown", nil))
		return DeployResult{}, temporal.NewNonRetryableApplicationError("deployment result is unknown", string(ErrorUnknownExternalState), &ClassifiedError{Class: ErrorUnknownExternalState, Operation: "deploy", Err: err})
	}
	reserved.Status = store.ExecutionSucceeded
	reserved.ExternalExecutionID = deployment.ExternalID
	reserved.Payload, err = encodeDeploymentResult(*run, reserved, deployment)
	if err != nil {
		return DeployResult{}, resultUnknown(err)
	}
	reserved.UpdatedAt = a.Now().UTC()
	successAudit := a.auditFor(input.RunID, input.RequestedBy, AuditIntent{ActorType: "system", ActorID: "workflow-worker", Action: "deployment.result", ResourceType: "service", ResourceID: input.Service, Result: "SUCCEEDED", Details: map[string]any{"external_execution_id": deployment.ExternalID}}, reserved.UpdatedAt)
	if err := a.Store.CompleteExecution(reserved, reserved.StateVersion, successAudit, a.outbox("execution", reserved.ID, "execution.succeeded", nil)); err != nil {
		return DeployResult{}, temporal.NewNonRetryableApplicationError("deployment succeeded but persistence is unknown", string(ErrorUnknownExternalState), err)
	}
	return DeployResult{Execution: domain.Execution{ID: reserved.ID, Adapter: reserved.Adapter, IdempotencyKey: reserved.IdempotencyKey, Status: string(reserved.Status), ExternalExecutionID: deployment.ExternalID, StartedAt: deployment.StartedAt, FinishedAt: deployment.FinishedAt}, Deployment: deployment}, nil
}

func (a *Activities) Verify(ctx context.Context, input VerifyInput) (executor.VerificationResult, error) {
	result, err := a.Executor.Verify(ctx, input.Deployment)
	if err != nil {
		return executor.VerificationResult{}, &ClassifiedError{Class: ErrorRetryable, Operation: "verify", Err: err}
	}
	if result.Evidence.Status != result.Outcome() {
		return executor.VerificationResult{}, &ClassifiedError{Class: ErrorTerminal, Operation: "verify", Err: errors.New("verification outcome and evidence disagree")}
	}
	if err := verificationcore.ValidateEvidence(result.Evidence); err != nil {
		return executor.VerificationResult{}, &ClassifiedError{Class: ErrorTerminal, Operation: "verify", Err: err}
	}
	return result, nil
}

func (a *Activities) Rollback(ctx context.Context, input RollbackInput) (RollbackResult, error) {
	run, err := a.Store.GetRun(input.Deploy.RunID)
	if err != nil {
		return RollbackResult{}, err
	}
	if err := validateProductionVerification(*run); err != nil {
		return RollbackResult{}, verificationGuardError(err)
	}
	if string(run.Environment) != input.Deploy.Environment {
		return RollbackResult{}, verificationGuardError(errors.New("rollback environment differs from durable run"))
	}
	source, err := a.Store.GetExecution(a.AdapterName, input.Deploy.IdempotencyKey)
	if err != nil {
		return RollbackResult{}, resultUnknown(err)
	}
	restored, err := restoreDeploymentResult(*run, source, store.ExecutionRecord{RunID: input.Deploy.RunID, Service: input.Deploy.Service, Adapter: a.AdapterName, IdempotencyKey: input.Deploy.IdempotencyKey})
	if err != nil {
		return RollbackResult{}, err
	}
	if !reflect.DeepEqual(restored.Deployment, input.Deployment) || restored.Deployment.DesiredVersion != input.Deploy.DesiredVersion {
		return RollbackResult{}, resultUnknown(errors.New("rollback source differs from durable deployment"))
	}
	key := input.Deploy.IdempotencyKey + "/rollback"
	now := a.Now().UTC()
	record := store.ExecutionRecord{ID: newID("rollback"), RunID: input.Deploy.RunID, Service: input.Deploy.Service, Adapter: a.AdapterName + ".rollback", IdempotencyKey: key, CreatedAt: now, UpdatedAt: now}
	audit := a.auditFor(input.Deploy.RunID, input.Deploy.RequestedBy, AuditIntent{ActorType: "system", ActorID: "workflow-worker", Action: "rollback.reserve", ResourceType: "service", ResourceID: input.Deploy.Service, Result: "AUTHORIZED"}, now)
	reserved, created, err := a.Store.ReserveExecution(record, audit, a.outbox("execution", record.ID, "rollback.reserved", nil))
	if err != nil {
		return RollbackResult{}, err
	}
	if !created {
		if reserved.Status == store.ExecutionSucceeded {
			result, err := restoreDeploymentResult(*run, reserved, record)
			return RollbackResult(result), err
		}
		return RollbackResult{}, temporal.NewNonRetryableApplicationError("rollback state requires reconciliation", string(ErrorUnknownExternalState), store.ErrUnknownExternalState)
	}
	deployment, err := a.Executor.Rollback(ctx, input.Deployment)
	if err != nil {
		reserved.Status = store.ExecutionUnknown
		reserved.UpdatedAt = a.Now().UTC()
		_ = a.Store.CompleteExecution(reserved, reserved.StateVersion, a.auditFor(input.Deploy.RunID, input.Deploy.RequestedBy, AuditIntent{ActorType: "system", ActorID: "workflow-worker", Action: "rollback.result", ResourceType: "service", ResourceID: input.Deploy.Service, Result: "UNKNOWN"}, reserved.UpdatedAt), a.outbox("execution", reserved.ID, "rollback.unknown", nil))
		return RollbackResult{}, temporal.NewNonRetryableApplicationError("rollback result is unknown", string(ErrorUnknownExternalState), err)
	}
	reserved.Status = store.ExecutionSucceeded
	reserved.ExternalExecutionID = deployment.ExternalID
	reserved.Payload, err = encodeDeploymentResult(*run, reserved, deployment)
	if err != nil {
		return RollbackResult{}, resultUnknown(err)
	}
	reserved.UpdatedAt = a.Now().UTC()
	if err := a.Store.CompleteExecution(reserved, reserved.StateVersion, a.auditFor(input.Deploy.RunID, input.Deploy.RequestedBy, AuditIntent{ActorType: "system", ActorID: "workflow-worker", Action: "rollback.result", ResourceType: "service", ResourceID: input.Deploy.Service, Result: "SUCCEEDED", Details: map[string]any{"external_execution_id": deployment.ExternalID}}, reserved.UpdatedAt), a.outbox("execution", reserved.ID, "rollback.succeeded", nil)); err != nil {
		return RollbackResult{}, temporal.NewNonRetryableApplicationError("rollback succeeded but persistence is unknown", string(ErrorUnknownExternalState), err)
	}
	return RollbackResult{Execution: domain.Execution{ID: reserved.ID, Adapter: reserved.Adapter, IdempotencyKey: reserved.IdempotencyKey, Status: string(reserved.Status), ExternalExecutionID: deployment.ExternalID, StartedAt: deployment.StartedAt, FinishedAt: deployment.FinishedAt}, Deployment: deployment}, nil
}

func (a *Activities) audit(run domain.ReleaseRun, intent AuditIntent) domain.AuditEvent {
	return a.auditFor(run.ID, run.RequestedBy, intent, run.UpdatedAt)
}
func (a *Activities) auditFor(runID, delegatedBy string, intent AuditIntent, at time.Time) domain.AuditEvent {
	return domain.AuditEvent{ID: newID("audit"), CorrelationID: runID, ActorType: intent.ActorType, ActorID: intent.ActorID, DelegatedBy: delegatedBy, Action: intent.Action, ResourceType: intent.ResourceType, ResourceID: intent.ResourceID, Result: intent.Result, Details: intent.Details, Timestamp: at}
}
func (a *Activities) outbox(prefix, aggregateID, eventType string, payload map[string]any) domain.OutboxEvent {
	now := a.Now().UTC()
	return domain.OutboxEvent{ID: newID(prefix), AggregateType: prefix, AggregateID: aggregateID, EventType: eventType, Payload: payload, CreatedAt: now, AvailableAt: now}
}

func newID(prefix string) string {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(data))
}
