package scheduler

import (
	"context"
	"errors"
	"time"

	"themisy/internal/domain"
)

var (
	ErrLeaseConflict = errors.New("schedule reservation owner or fencing token changed")
	ErrUnresolved    = errors.New("schedule reservation has an unresolved operation")
	ErrConfiguration = errors.New("schedule coordinator configuration changed")
)

// ScheduleLease is the Control Plane's durable ownership record for one Run
// step. It grants no provider authority and is not a Runner credential.
type ScheduleLease struct {
	ID         string    `json:"id,omitempty"`
	RunID      string    `json:"run_id,omitempty"`
	StepID     string    `json:"step_id,omitempty"`
	OwnerToken string    `json:"owner_token,omitempty"`
	Fence      int64     `json:"fence,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
}

type ScheduleOutcome string

const (
	ScheduleOutcomeSucceeded ScheduleOutcome = "SUCCEEDED"
	ScheduleOutcomeFailed    ScheduleOutcome = "FAILED"
	ScheduleOutcomeUnknown   ScheduleOutcome = "UNKNOWN"
)

type AcquireRequest struct {
	ReservationID string
	RunID         string
	Step          Step
	Constraints   []Constraint
	Circuit       CircuitPolicy
	OwnerToken    string
	Now           time.Time
	LeaseUntil    time.Time
	Audit         domain.AuditEvent
}

type AdmissionRequest struct {
	Lease        ScheduleLease
	OperationKey string
	Now          time.Time
	Audit        domain.AuditEvent
}

type CompletionRequest struct {
	Lease   ScheduleLease
	Outcome ScheduleOutcome
	Circuit CircuitPolicy
	Now     time.Time
	Audit   domain.AuditEvent
}

type QuarantineRequest struct {
	ReservationID string
	RunID         string
	Step          Step
	Constraints   []Constraint
	Circuit       CircuitPolicy
	OperationKey  string
	OwnerToken    string
	Now           time.Time
	Audit         domain.AuditEvent
}

// DurableCoordinator is optional for compatibility/test stores. Production
// PostgreSQL implements it; callers fail closed when an implementation is
// present but unavailable.
type DurableCoordinator interface {
	AcquireSchedule(context.Context, AcquireRequest) (ScheduleLease, []Blocked, CircuitState, error)
	AdmitSchedule(context.Context, AdmissionRequest) error
	CompleteSchedule(context.Context, CompletionRequest) (CircuitState, error)
	QuarantineSchedule(context.Context, QuarantineRequest) error
	GetCircuitState(context.Context, string) (CircuitState, error)
}
