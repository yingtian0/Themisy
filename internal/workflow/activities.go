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
	"themisy/pkg/protocol"

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
	ActivityRecoverExecution = "RecoverExecution"
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
	ScheduleLease  *scheduler.ScheduleLease `json:"schedule_lease,omitempty"`
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

type RecoverExecutionInput struct {
	RunID          string
	Service        string
	Environment    string
	DesiredVersion string
	IdempotencyKey string
	Rollback       bool
}

type RecoverExecutionResult struct {
	Supported  bool
	Ready      bool
	Failed     bool
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
	Lease     *scheduler.ScheduleLease `json:"lease,omitempty"`
}

type ScheduleCompleteInput struct {
	RunID   string
	StepID  string
	Failed  bool
	Unknown bool                     `json:"unknown,omitempty"`
	Lease   *scheduler.ScheduleLease `json:"lease,omitempty"`
}

type Activities struct {
	Store            store.DurableStore
	Policy           *policy.Engine
	Executor         executor.ReleaseExecutor
	AdapterName      string
	Now              func() time.Time
	Scheduler        *scheduler.Scheduler
	Capacity         func(ScheduleInput) scheduler.Capacity
	ScheduleLimits   scheduler.Limits
	CircuitPolicy    scheduler.CircuitPolicy
	ScheduleLeaseTTL time.Duration
	scheduleMu       sync.Mutex
	scheduled        map[string]scheduler.Dispatch
}

func NewActivities(st store.DurableStore, policyEngine *policy.Engine, releaseExecutor executor.ReleaseExecutor) *Activities {
	coordinator, err := scheduler.New(scheduler.DefaultLimits(), scheduler.CircuitPolicy{MinimumSamples: 5, WindowSize: 20, ErrorRate: .5})
	if err != nil {
		panic(err)
	}
	return &Activities{Store: st, Policy: policyEngine, Executor: releaseExecutor, AdapterName: "mock", Now: time.Now, Scheduler: coordinator, ScheduleLimits: scheduler.DefaultLimits(), CircuitPolicy: scheduler.CircuitPolicy{MinimumSamples: 5, WindowSize: 20, ErrorRate: .5}, ScheduleLeaseTTL: 5 * time.Minute, Capacity: func(ScheduleInput) scheduler.Capacity {
		return scheduler.Capacity{RunnerAvailable: 20, AdapterRemaining: 20, QueueLimit: 100}
	}, scheduled: make(map[string]scheduler.Dispatch)}
}

func (a *Activities) AcquireSchedule(ctx context.Context, input ScheduleInput) (ScheduleResult, error) {
	if coordinator, ok := a.Store.(scheduler.DurableCoordinator); ok {
		capacity := scheduler.Capacity{}
		if a.Capacity != nil {
			capacity = a.Capacity(input)
		}
		available, pressure := capacity.Available()
		if available == 0 {
			now := a.Now().UTC()
			if err := a.Store.AppendAudit(a.auditFor(input.RunID, "scheduler", AuditIntent{ActorType: "system", ActorID: "scheduler", Action: "schedule.block", ResourceType: "release_step", ResourceID: input.Step.ID, Result: "BLOCKED", Details: map[string]any{"reason": string(pressure)}}, now)); err != nil {
				return ScheduleResult{Reason: "SHARED_COORDINATOR_UNAVAILABLE"}, nil
			}
			return ScheduleResult{Reason: string(pressure)}, nil
		}
		constraints, err := scheduler.ConstraintsFor(input.Step, a.ScheduleLimits)
		if err != nil {
			_ = a.Store.AppendAudit(a.auditFor(input.RunID, "scheduler", AuditIntent{ActorType: "system", ActorID: "scheduler", Action: "schedule.block", ResourceType: "release_step", ResourceID: input.Step.ID, Result: "BLOCKED", Details: map[string]any{"reason": "SCHEDULE_COORDINATOR_CONFIGURATION"}}, a.Now().UTC()))
			return ScheduleResult{Reason: "SCHEDULE_COORDINATOR_CONFIGURATION"}, nil
		}
		now := a.Now().UTC()
		leaseTTL := a.ScheduleLeaseTTL
		if leaseTTL <= 0 {
			leaseTTL = 5 * time.Minute
		}
		lease, blocked, circuit, err := coordinator.AcquireSchedule(ctx, scheduler.AcquireRequest{
			ReservationID: newID("schedule"), RunID: input.RunID, Step: input.Step, Constraints: constraints,
			Circuit: a.CircuitPolicy, OwnerToken: newID("schedule-owner"), Now: now, LeaseUntil: now.Add(leaseTTL),
			Audit: a.auditFor(input.RunID, "scheduler", AuditIntent{ActorType: "system", ActorID: "scheduler", Action: "schedule.acquire", ResourceType: "release_step", ResourceID: input.Step.ID, Result: "REQUESTED"}, now),
		})
		if err != nil {
			_ = a.Store.AppendAudit(a.auditFor(input.RunID, "scheduler", AuditIntent{ActorType: "system", ActorID: "scheduler", Action: "schedule.block", ResourceType: "release_step", ResourceID: input.Step.ID, Result: "BLOCKED", Details: map[string]any{"reason": "SHARED_COORDINATOR_UNAVAILABLE"}}, a.Now().UTC()))
			return ScheduleResult{Reason: "SHARED_COORDINATOR_UNAVAILABLE"}, nil
		}
		if len(blocked) > 0 {
			reason := "CONCURRENCY_BUDGET_EXHAUSTED"
			if circuit.Open {
				reason = "CIRCUIT_BREAKER_OPEN"
			}
			return ScheduleResult{Reason: reason, BlockedBy: blocked}, nil
		}
		return ScheduleResult{Allowed: true, Lease: &lease}, nil
	}
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

func (a *Activities) CompleteSchedule(ctx context.Context, input ScheduleCompleteInput) (scheduler.CircuitState, error) {
	if coordinator, ok := a.Store.(scheduler.DurableCoordinator); ok {
		if input.Lease == nil {
			return scheduler.CircuitState{}, nil
		}
		outcome := scheduler.ScheduleOutcomeSucceeded
		if input.Unknown {
			outcome = scheduler.ScheduleOutcomeUnknown
		} else if input.Failed {
			outcome = scheduler.ScheduleOutcomeFailed
		}
		now := a.Now().UTC()
		return coordinator.CompleteSchedule(ctx, scheduler.CompletionRequest{
			Lease: *input.Lease, Outcome: outcome, Circuit: a.CircuitPolicy, Now: now,
			Audit: a.auditFor(input.RunID, "scheduler", AuditIntent{ActorType: "system", ActorID: "scheduler", Action: "schedule.complete", ResourceType: "release_step", ResourceID: input.StepID, Result: string(outcome)}, now),
		})
	}
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
			if getErr == nil {
				return domain.ReleaseRun{}, fmt.Errorf("release run state version conflict: expected %d, current %d (incoming status %s, durable status %s): %w", expected, current.StateVersion, input.Run.Status, current.Status, err)
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
	if coordinator, ok := a.Store.(scheduler.DurableCoordinator); ok {
		existing, found, lookupErr := a.executionBeforeDispatch(record.Adapter, record.IdempotencyKey)
		if lookupErr != nil {
			return DeployResult{}, lookupErr
		}
		if input.ScheduleLease == nil || input.ScheduleLease.OwnerToken == "" {
			if !found || existing.Status != store.ExecutionSucceeded {
				if err := a.quarantineLegacySchedule(ctx, coordinator, *run, input.Service, input.IdempotencyKey, now); err != nil {
					return DeployResult{}, err
				}
				return DeployResult{}, temporal.NewNonRetryableApplicationError("schedule reservation is missing; execution requires reconciliation", string(ErrorUnknownExternalState), store.ErrUnknownExternalState)
			}
		} else if found && existing.Status == store.ExecutionSucceeded {
			// Restore the durable idempotent result below without reopening write admission.
		} else {
			if err := coordinator.AdmitSchedule(ctx, scheduler.AdmissionRequest{Lease: *input.ScheduleLease, OperationKey: input.IdempotencyKey, Now: now,
				Audit: a.auditFor(input.RunID, input.RequestedBy, AuditIntent{ActorType: "system", ActorID: "scheduler", Action: "schedule.admit", ResourceType: "release_step", ResourceID: input.Service, Result: "AUTHORIZED"}, now)}); err != nil {
				if errors.Is(err, scheduler.ErrLeaseConflict) || errors.Is(err, scheduler.ErrUnresolved) {
					return DeployResult{}, temporal.NewNonRetryableApplicationError("schedule reservation is no longer writable", string(ErrorUnknownExternalState), err)
				}
				return DeployResult{}, err
			}
		}
	}
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
		if grantID := pendingGrantID(err); grantID != "" {
			reserved.Payload["grant_id"] = grantID
		}
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

func pendingGrantID(err error) string {
	var identified interface{ GrantID() string }
	if errors.As(err, &identified) {
		return identified.GrantID()
	}
	return ""
}

// RecoverExecution observes the already-issued Grant and persists only its
// signed, terminal result. It never issues a Grant or invokes a provider write.
func (a *Activities) RecoverExecution(ctx context.Context, input RecoverExecutionInput) (RecoverExecutionResult, error) {
	adapterName := a.AdapterName
	capability := "release.deploy"
	key := input.IdempotencyKey
	if input.Rollback {
		adapterName += ".rollback"
		capability = "release.rollback"
		key += "/rollback"
	}
	run, err := a.Store.GetRun(input.RunID)
	if err != nil {
		return RecoverExecutionResult{}, err
	}
	if run.Status == domain.RunCancelled || run.Status == domain.RunSucceeded || run.Status == domain.RunFailed || run.Status == domain.RunEscalated {
		return RecoverExecutionResult{}, resultUnknown(errors.New("run is terminal; unresolved execution remains quarantined"))
	}
	stepUnknown := false
	for _, step := range run.Steps {
		if step.Service == input.Service && step.Status == domain.StepUnknown {
			stepUnknown = true
			break
		}
	}
	if run.Status != domain.RunBlocked || !stepUnknown {
		return RecoverExecutionResult{}, resultUnknown(errors.New("run step is no longer awaiting recovery"))
	}
	record, err := a.Store.GetExecution(adapterName, key)
	if err != nil {
		return RecoverExecutionResult{}, err
	}
	if record.RunID != input.RunID || record.Service != input.Service {
		return RecoverExecutionResult{}, resultUnknown(errors.New("execution does not match recovery request"))
	}
	if record.Status == store.ExecutionSucceeded {
		result, err := restoreDeploymentResult(*run, record, store.ExecutionRecord{RunID: input.RunID, Service: input.Service, Adapter: adapterName, IdempotencyKey: key})
		if err != nil {
			return RecoverExecutionResult{}, err
		}
		return RecoverExecutionResult{Supported: true, Ready: true, Execution: result.Execution, Deployment: result.Deployment}, nil
	}
	if record.Status == store.ExecutionFailed {
		grantID, ok := record.Payload["grant_id"].(string)
		if !ok || grantID == "" {
			return RecoverExecutionResult{}, resultUnknown(errors.New("failed execution has no durable Grant binding"))
		}
		dispatch, err := a.Store.GetGrantDispatch(ctx, grantID)
		if err != nil {
			return RecoverExecutionResult{}, err
		}
		if dispatch.Status != store.GrantDispatchRejected || dispatch.Grant.GrantID != grantID || dispatch.Grant.RunID != input.RunID || dispatch.Grant.StepID != input.Service || dispatch.Grant.PlanHash != run.Plan.Hash || dispatch.Grant.IdempotencyKey != key || dispatch.Result.GrantID != grantID || dispatch.Result.RunID != input.RunID || dispatch.Result.StepID != input.Service || dispatch.Result.Status != protocol.ResultRejected {
			return RecoverExecutionResult{}, resultUnknown(errors.New("failed execution does not match a rejected durable Grant"))
		}
		return RecoverExecutionResult{Supported: true, Ready: true, Failed: true, Execution: domain.Execution{ID: record.ID, Adapter: adapterName, IdempotencyKey: key, Status: string(record.Status)}}, nil
	}
	if record.Status != store.ExecutionUnknown {
		return RecoverExecutionResult{}, resultUnknown(errors.New("execution is not awaiting reconciliation"))
	}
	grantID, ok := record.Payload["grant_id"].(string)
	if !ok || grantID == "" {
		return RecoverExecutionResult{Ready: false}, nil
	}
	result := RecoverExecutionResult{Supported: true}
	dispatch, err := a.Store.GetGrantDispatch(ctx, grantID)
	if err != nil {
		return RecoverExecutionResult{}, err
	}
	grant := dispatch.Grant
	plannedRunnerGroup := ""
	for _, phase := range run.Plan.Phases {
		for _, planned := range phase.Steps {
			if planned.Service == input.Service && planned.DesiredVersion == input.DesiredVersion {
				plannedRunnerGroup = planned.Scheduling.RunnerGroup
			}
		}
	}
	if grant.GrantID != grantID || grant.RunID != input.RunID || grant.StepID != input.Service || grant.TenantID != run.TenantID || grant.RunnerGroup == "" || grant.RunnerGroup != plannedRunnerGroup || grant.Target.Service != input.Service || grant.Target.Environment != input.Environment || grant.PlanHash != run.Plan.Hash || grant.IdempotencyKey != key || string(grant.Action.Capability) != capability || grant.Action.ArtifactDigest != input.DesiredVersion {
		return RecoverExecutionResult{}, resultUnknown(errors.New("Grant binding does not match unresolved execution"))
	}
	if input.Rollback {
		source, err := a.Store.GetExecution(a.AdapterName, input.IdempotencyKey)
		if err != nil {
			return RecoverExecutionResult{}, err
		}
		sourceResult, err := restoreDeploymentResult(*run, source, store.ExecutionRecord{RunID: input.RunID, Service: input.Service, Adapter: a.AdapterName, IdempotencyKey: input.IdempotencyKey})
		if err != nil || grant.Action.ExternalExecutionID != sourceResult.Deployment.ExternalID {
			return RecoverExecutionResult{}, resultUnknown(errors.New("rollback Grant does not match the durable source deployment"))
		}
	}
	switch dispatch.Status {
	case store.GrantDispatchUnknown, store.GrantDispatchPending, store.GrantDispatchLeased, store.GrantDispatchAcked:
		return result, nil
	case store.GrantDispatchRejected:
		record.Status = store.ExecutionFailed
		record.UpdatedAt = a.Now().UTC()
		record.Payload = map[string]any{"grant_id": grantID, "recovery_status": string(dispatch.Result.Status), "recovery_reason": dispatch.Result.ReasonCode}
		audit := a.auditFor(input.RunID, run.RequestedBy, AuditIntent{ActorType: "system", ActorID: "workflow-recovery", Action: "execution.recovered", ResourceType: "service", ResourceID: input.Service, Result: "FAILED"}, record.UpdatedAt)
		if err := a.Store.CompleteExecution(record, record.StateVersion, audit, a.outbox("execution", record.ID, "execution.recovered.failed", nil)); err != nil {
			return RecoverExecutionResult{}, err
		}
		result.Ready, result.Failed = true, true
		result.Execution = domain.Execution{ID: record.ID, Adapter: adapterName, IdempotencyKey: key, Status: string(record.Status)}
		return result, nil
	case store.GrantDispatchSucceeded:
	default:
		return RecoverExecutionResult{}, resultUnknown(errors.New("unsupported Grant dispatch state"))
	}
	if dispatch.Result.GrantID != grantID || dispatch.Result.RunID != input.RunID || dispatch.Result.StepID != input.Service || dispatch.Result.CompletedAt.IsZero() {
		return RecoverExecutionResult{}, resultUnknown(errors.New("terminal Grant result binding is invalid"))
	}
	if dispatch.Status == store.GrantDispatchRejected && dispatch.Result.Status != protocol.ResultRejected {
		return RecoverExecutionResult{}, resultUnknown(errors.New("rejected Grant result has an invalid status"))
	}
	if dispatch.Status == store.GrantDispatchSucceeded && dispatch.Result.Status != protocol.ResultSucceeded {
		return RecoverExecutionResult{}, resultUnknown(errors.New("successful Grant result has an invalid status"))
	}
	deployment := executor.Deployment{GrantID: grantID, ExternalID: dispatch.Result.ExternalExecutionID, StartedAt: grant.IssuedAt, FinishedAt: dispatch.Result.CompletedAt, RunID: input.RunID, Service: input.Service, Environment: input.Environment, DesiredVersion: input.DesiredVersion, IdempotencyKey: key}
	record.Status = store.ExecutionSucceeded
	record.ExternalExecutionID = deployment.ExternalID
	record.Payload, err = encodeDeploymentResult(*run, record, deployment)
	if err != nil {
		return RecoverExecutionResult{}, resultUnknown(err)
	}
	record.UpdatedAt = a.Now().UTC()
	audit := a.auditFor(input.RunID, run.RequestedBy, AuditIntent{ActorType: "system", ActorID: "workflow-recovery", Action: "execution.recovered", ResourceType: "service", ResourceID: input.Service, Result: "SUCCEEDED", Details: map[string]any{"grant_id": grantID}}, record.UpdatedAt)
	if err := a.Store.CompleteExecution(record, record.StateVersion, audit, a.outbox("execution", record.ID, "execution.recovered.succeeded", nil)); err != nil {
		return RecoverExecutionResult{}, err
	}
	result.Ready = true
	result.Execution = domain.Execution{ID: record.ID, Adapter: adapterName, IdempotencyKey: key, Status: string(record.Status), ExternalExecutionID: deployment.ExternalID, StartedAt: deployment.StartedAt, FinishedAt: deployment.FinishedAt}
	result.Deployment = deployment
	return result, nil
}

func (a *Activities) executionBeforeDispatch(adapterName, idempotencyKey string) (store.ExecutionRecord, bool, error) {
	record, err := a.Store.GetExecution(adapterName, idempotencyKey)
	if errors.Is(err, store.ErrNotFound) {
		return store.ExecutionRecord{}, false, nil
	}
	if err != nil {
		return store.ExecutionRecord{}, false, err
	}
	return record, true, nil
}

func (a *Activities) quarantineLegacySchedule(ctx context.Context, coordinator scheduler.DurableCoordinator, run domain.ReleaseRun, service, operationKey string, now time.Time) error {
	step := scheduler.Step{ID: service, Tenant: run.TenantID, Environment: string(run.Environment), Region: run.Region, Cluster: run.Cluster}
	for _, releaseStep := range run.Steps {
		if releaseStep.Service == service {
			step = schedulingStep(run, releaseStep)
			break
		}
	}
	constraints, err := scheduler.ConstraintsFor(step, a.ScheduleLimits)
	if err != nil {
		return fmt.Errorf("build legacy schedule quarantine constraints: %w", err)
	}
	return coordinator.QuarantineSchedule(ctx, scheduler.QuarantineRequest{
		ReservationID: newID("schedule-quarantine"), RunID: run.ID, Step: step, Constraints: constraints,
		Circuit: a.CircuitPolicy, OperationKey: operationKey, OwnerToken: newID("schedule-owner"), Now: now,
		Audit: a.auditFor(run.ID, run.RequestedBy, AuditIntent{ActorType: "system", ActorID: "scheduler", Action: "schedule.quarantine", ResourceType: "release_step", ResourceID: service, Result: "UNKNOWN"}, now),
	})
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
	if coordinator, ok := a.Store.(scheduler.DurableCoordinator); ok {
		existing, found, lookupErr := a.executionBeforeDispatch(record.Adapter, key)
		if lookupErr != nil {
			return RollbackResult{}, lookupErr
		}
		if input.Deploy.ScheduleLease == nil || input.Deploy.ScheduleLease.OwnerToken == "" {
			if !found || existing.Status != store.ExecutionSucceeded {
				if err := a.quarantineLegacySchedule(ctx, coordinator, *run, input.Deploy.Service, key, now); err != nil {
					return RollbackResult{}, err
				}
				return RollbackResult{}, temporal.NewNonRetryableApplicationError("schedule reservation is missing; rollback requires reconciliation", string(ErrorUnknownExternalState), store.ErrUnknownExternalState)
			}
		} else if found && existing.Status == store.ExecutionSucceeded {
			// Restore the durable idempotent result below without reopening write admission.
		} else if err := coordinator.AdmitSchedule(ctx, scheduler.AdmissionRequest{Lease: *input.Deploy.ScheduleLease, OperationKey: key, Now: now,
			Audit: a.auditFor(input.Deploy.RunID, input.Deploy.RequestedBy, AuditIntent{ActorType: "system", ActorID: "scheduler", Action: "schedule.admit", ResourceType: "release_step", ResourceID: input.Deploy.Service, Result: "AUTHORIZED"}, now)}); err != nil {
			if errors.Is(err, scheduler.ErrLeaseConflict) || errors.Is(err, scheduler.ErrUnresolved) {
				return RollbackResult{}, temporal.NewNonRetryableApplicationError("schedule reservation is no longer writable", string(ErrorUnknownExternalState), err)
			}
			return RollbackResult{}, err
		}
	}
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
		if grantID := pendingGrantID(err); grantID != "" {
			if reserved.Payload == nil {
				reserved.Payload = map[string]any{}
			}
			reserved.Payload["grant_id"] = grantID
		}
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
