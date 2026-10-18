package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"themisy/internal/domain"
	"themisy/internal/scheduler"

	"github.com/jackc/pgx/v5"
)

var (
	ErrScheduleLeaseConflict = scheduler.ErrLeaseConflict
	ErrScheduleUnresolved    = scheduler.ErrUnresolved
	ErrScheduleConfiguration = scheduler.ErrConfiguration
)

const scheduleLeaseState = `SELECT id,tenant_id,environment,step_payload,owner_token,fence,status,lease_expires_at
FROM workflow_schedule_reservations WHERE run_id=$1 AND step_id=$2 FOR UPDATE`

func (s *Store) AcquireSchedule(ctx context.Context, request scheduler.AcquireRequest) (scheduler.ScheduleLease, []scheduler.Blocked, scheduler.CircuitState, error) {
	if request.ReservationID == "" || request.RunID == "" || request.Step.ID == "" || request.OwnerToken == "" || request.Now.IsZero() || !request.LeaseUntil.After(request.Now) {
		return scheduler.ScheduleLease{}, nil, scheduler.CircuitState{}, errors.New("invalid schedule reservation request")
	}
	constraints, err := normalizeConstraints(request.Constraints)
	if err != nil {
		return scheduler.ScheduleLease{}, nil, scheduler.CircuitState{}, err
	}
	request.Step = normalizeScheduleStep(request.Step)
	// PostgreSQL timestamptz stores microsecond precision. Normalize the
	// lease deadline before persisting and returning it so fencing checks
	// compare the same durable value across independent Store instances.
	request.LeaseUntil = request.LeaseUntil.UTC().Truncate(time.Microsecond)
	stepPayload, err := json.Marshal(request.Step)
	if err != nil {
		return scheduler.ScheduleLease{}, nil, scheduler.CircuitState{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return scheduler.ScheduleLease{}, nil, scheduler.CircuitState{}, err
	}
	defer tx.Rollback(ctx)
	lockKey := reservationLockKey(request.RunID, request.Step.ID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return scheduler.ScheduleLease{}, nil, scheduler.CircuitState{}, err
	}
	tenant := coordinationTenant(request.Step.Tenant)
	circuit := scheduler.CircuitState{}
	var existingID, existingTenant, existingEnvironment, existingOwner, existingStatus string
	var existingPayload []byte
	var fence int64
	var leaseUntil time.Time
	err = tx.QueryRow(ctx, scheduleLeaseState, request.RunID, request.Step.ID).Scan(&existingID, &existingTenant, &existingEnvironment, &existingPayload, &existingOwner, &fence, &existingStatus, &leaseUntil)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return scheduler.ScheduleLease{}, nil, circuit, err
	}
	reservationNotFound := errors.Is(err, pgx.ErrNoRows)
	if err == nil {
		if existingTenant != tenant || existingEnvironment != request.Step.Environment || !sameScheduleStep(existingPayload, request.Step) {
			return scheduler.ScheduleLease{}, nil, circuit, ErrScheduleConfiguration
		}
		switch existingStatus {
		case "RESERVED":
			if err := verifyScheduleScopes(ctx, tx, existingID, constraints); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			if request.Now.Before(leaseUntil) {
				if err := tx.Commit(ctx); err != nil {
					return scheduler.ScheduleLease{}, nil, circuit, err
				}
				return scheduler.ScheduleLease{ID: existingID, RunID: request.RunID, StepID: request.Step.ID, OwnerToken: existingOwner, Fence: fence, ExpiresAt: leaseUntil}, nil, circuit, nil
			}
			if err := reactivateScheduleScopes(ctx, tx, existingID, constraints); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			fence++
			if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_reservations SET owner_token=$2,fence=$3,lease_expires_at=$4,updated_at=$5 WHERE id=$1`, existingID, request.OwnerToken, fence, request.LeaseUntil, request.Now); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "EXPIRED_UNADMITTED_RESERVATION_RECLAIMED", "fence": fence}); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			if err := tx.Commit(ctx); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			return scheduler.ScheduleLease{ID: existingID, RunID: request.RunID, StepID: request.Step.ID, OwnerToken: request.OwnerToken, Fence: fence, ExpiresAt: request.LeaseUntil}, nil, circuit, nil
		case "ADMITTED", "UNKNOWN":
			if err := verifyScheduleScopes(ctx, tx, existingID, constraints); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "SCHEDULE_RESERVATION_UNRESOLVED", "status": existingStatus}); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			if err := tx.Commit(ctx); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			return scheduler.ScheduleLease{}, []scheduler.Blocked{{Dimension: scheduler.DimensionService, Key: tenant + "/" + request.Step.Environment + "/" + request.Step.ID, Limit: 1}}, circuit, nil
		case "SUCCEEDED", "FAILED":
			if err := verifyScheduleScopes(ctx, tx, existingID, constraints); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "SCHEDULE_ALREADY_COMPLETED", "status": existingStatus}); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			if err := tx.Commit(ctx); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			return scheduler.ScheduleLease{}, []scheduler.Blocked{{Dimension: scheduler.DimensionService, Key: tenant + "/" + request.Step.Environment + "/" + request.Step.ID, Limit: 1}}, circuit, nil
		default:
			return scheduler.ScheduleLease{}, nil, circuit, fmt.Errorf("invalid schedule reservation status: %s", existingStatus)
		}
	}
	if request.Step.Environment == "production" && reservationNotFound {
		circuit, err = lockWorkflowCircuit(ctx, tx, tenant, request.Step.Environment, request.Circuit, request.Now)
		if err != nil {
			return scheduler.ScheduleLease{}, nil, circuit, err
		}
		if circuit.Open {
			if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "CIRCUIT_OPEN", "tenant": tenant}); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			if err := tx.Commit(ctx); err != nil {
				return scheduler.ScheduleLease{}, nil, circuit, err
			}
			return scheduler.ScheduleLease{}, []scheduler.Blocked{{Dimension: scheduler.DimensionTenant, Key: tenant, Limit: 1}}, circuit, nil
		}
	}
	if !reservationNotFound {
		return scheduler.ScheduleLease{}, nil, circuit, errors.New("schedule reservation lookup state changed unexpectedly")
	}
	blocked, err := lockAndCheckScheduleCounters(ctx, tx, constraints)
	if err != nil {
		return scheduler.ScheduleLease{}, nil, circuit, err
	}
	if len(blocked) > 0 {
		if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "CONCURRENCY_BUDGET_EXHAUSTED", "blocked_by": blocked}); err != nil {
			return scheduler.ScheduleLease{}, nil, circuit, err
		}
		if err := tx.Commit(ctx); err != nil {
			return scheduler.ScheduleLease{}, nil, circuit, err
		}
		return scheduler.ScheduleLease{}, blocked, circuit, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_reservations
(id,run_id,step_id,tenant_id,environment,step_payload,owner_token,fence,status,lease_expires_at,created_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,1,'RESERVED',$8,$9,$9)`, request.ReservationID, request.RunID, request.Step.ID, tenant, request.Step.Environment, stepPayload, request.OwnerToken, request.LeaseUntil, request.Now); err != nil {
		return scheduler.ScheduleLease{}, nil, circuit, err
	}
	if err := ensureScheduleCountersLocked(ctx, tx, constraints); err != nil {
		return scheduler.ScheduleLease{}, nil, circuit, err
	}
	if err := activateScheduleScopes(ctx, tx, request.ReservationID, constraints); err != nil {
		return scheduler.ScheduleLease{}, nil, circuit, err
	}
	if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "SCHEDULE_RESERVED", "fence": 1}); err != nil {
		return scheduler.ScheduleLease{}, nil, circuit, err
	}
	if err := tx.Commit(ctx); err != nil {
		return scheduler.ScheduleLease{}, nil, circuit, err
	}
	return scheduler.ScheduleLease{ID: request.ReservationID, RunID: request.RunID, StepID: request.Step.ID, OwnerToken: request.OwnerToken, Fence: 1, ExpiresAt: request.LeaseUntil}, nil, circuit, nil
}

func (s *Store) AdmitSchedule(ctx context.Context, request scheduler.AdmissionRequest) error {
	if request.OperationKey == "" || request.Now.IsZero() || request.Lease.ID == "" {
		return ErrScheduleLeaseConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var tenant, environment, token, status string
	var runID, stepID string
	var fence int64
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT run_id,step_id,tenant_id,environment,owner_token,fence,status,lease_expires_at
FROM workflow_schedule_reservations WHERE id=$1 FOR UPDATE`, request.Lease.ID).Scan(&runID, &stepID, &tenant, &environment, &token, &fence, &status, &expiresAt)
	if err != nil {
		return err
	}
	if !expiresAt.Equal(request.Lease.ExpiresAt) {
		return fmt.Errorf("admission expiry mismatch: database=%s lease=%s: %w", expiresAt.Format(time.RFC3339Nano), request.Lease.ExpiresAt.Format(time.RFC3339Nano), ErrScheduleLeaseConflict)
	}
	if runID != request.Lease.RunID || stepID != request.Lease.StepID || token != request.Lease.OwnerToken || fence != request.Lease.Fence {
		return fmt.Errorf("admission identity mismatch: run=%q/%q step=%q/%q token=%t fence=%d/%d: %w", runID, request.Lease.RunID, stepID, request.Lease.StepID, token == request.Lease.OwnerToken, fence, request.Lease.Fence, ErrScheduleLeaseConflict)
	}
	if status != "RESERVED" && status != "ADMITTED" {
		return fmt.Errorf("admission state %s is not writable: %w", status, ErrScheduleUnresolved)
	}
	if status == "RESERVED" && !request.Now.Before(expiresAt) {
		return fmt.Errorf("unadmitted reservation expired at %s, now %s: %w", expiresAt, request.Now, ErrScheduleLeaseConflict)
	}
	if status == "ADMITTED" && !request.Now.Before(expiresAt) {
		return ErrScheduleLeaseConflict
	}
	if status == "ADMITTED" {
		var activeAdmissions int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_schedule_admissions WHERE reservation_id=$1 AND status='ADMITTED'`, request.Lease.ID).Scan(&activeAdmissions); err != nil {
			return err
		}
		if activeAdmissions == 0 {
			return ErrScheduleUnresolved
		}
	}
	var admissionStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM workflow_schedule_admissions WHERE reservation_id=$1 AND operation_key=$2`, request.Lease.ID, request.OperationKey).Scan(&admissionStatus)
	if err == nil {
		if admissionStatus == "ADMITTED" && status == "ADMITTED" {
			return tx.Commit(ctx)
		}
		return ErrScheduleUnresolved
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_admissions
(reservation_id,operation_key,fence,status,created_at,updated_at) VALUES ($1,$2,$3,'ADMITTED',$4,$4)`, request.Lease.ID, request.OperationKey, request.Lease.Fence, request.Now); err != nil {
		return err
	}
	if status == "RESERVED" {
		if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_reservations SET status='ADMITTED',updated_at=$2 WHERE id=$1`, request.Lease.ID, request.Now); err != nil {
			return err
		}
	}
	if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "WRITE_ADMITTED", "operation_key": request.OperationKey, "fence": request.Lease.Fence, "tenant": tenant, "environment": environment}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CompleteSchedule(ctx context.Context, request scheduler.CompletionRequest) (scheduler.CircuitState, error) {
	if request.Lease.ID == "" || request.Lease.OwnerToken == "" || request.Now.IsZero() {
		return scheduler.CircuitState{}, ErrScheduleLeaseConflict
	}
	if request.Outcome != scheduler.ScheduleOutcomeSucceeded && request.Outcome != scheduler.ScheduleOutcomeFailed && request.Outcome != scheduler.ScheduleOutcomeUnknown {
		return scheduler.CircuitState{}, errors.New("invalid schedule outcome")
	}
	if request.Lease.ExpiresAt.IsZero() {
		return scheduler.CircuitState{}, ErrScheduleLeaseConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return scheduler.CircuitState{}, err
	}
	defer tx.Rollback(ctx)
	var runID, stepID, tenant, environment, token, status string
	var fence int64
	err = tx.QueryRow(ctx, `SELECT run_id,step_id,tenant_id,environment,owner_token,fence,status
FROM workflow_schedule_reservations WHERE id=$1 FOR UPDATE`, request.Lease.ID).Scan(&runID, &stepID, &tenant, &environment, &token, &fence, &status)
	if err != nil {
		return scheduler.CircuitState{}, err
	}
	if runID != request.Lease.RunID || stepID != request.Lease.StepID || token != request.Lease.OwnerToken || fence != request.Lease.Fence {
		return scheduler.CircuitState{}, fmt.Errorf("completion lease mismatch: run=%q/%q step=%q/%q token=%t fence=%d/%d", runID, request.Lease.RunID, stepID, request.Lease.StepID, token == request.Lease.OwnerToken, fence, request.Lease.Fence)
	}
	// Once an admitted operation is UNKNOWN, no replacement reservation can
	// take its place. Its original owner may therefore record a later
	// read-only reconciliation after the write lease expires, provided the
	// owner token and fence still match the locked UNKNOWN row.
	if !request.Now.Before(request.Lease.ExpiresAt) && status != "UNKNOWN" && status != string(request.Outcome) {
		return scheduler.CircuitState{}, fmt.Errorf("completion lease expired at %s, now %s: %w", request.Lease.ExpiresAt, request.Now, ErrScheduleLeaseConflict)
	}
	circuit := scheduler.CircuitState{}
	if environment == "production" {
		circuit, err = lockWorkflowCircuit(ctx, tx, tenant, environment, request.Circuit, request.Now)
		if err != nil {
			return scheduler.CircuitState{}, err
		}
	}
	var admissionCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_schedule_admissions WHERE reservation_id=$1`, request.Lease.ID).Scan(&admissionCount); err != nil {
		return scheduler.CircuitState{}, err
	}
	if status == "SUCCEEDED" || status == "FAILED" {
		if request.Outcome == scheduler.ScheduleOutcomeUnknown || string(request.Outcome) != status {
			return scheduler.CircuitState{}, ErrScheduleUnresolved
		}
		if err := tx.Commit(ctx); err != nil {
			return scheduler.CircuitState{}, err
		}
		return circuit, nil
	}
	if request.Outcome == scheduler.ScheduleOutcomeUnknown {
		if status != "UNKNOWN" {
			if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_reservations SET status='UNKNOWN',updated_at=$2 WHERE id=$1`, request.Lease.ID, request.Now); err != nil {
				return scheduler.CircuitState{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_admissions SET status='UNKNOWN',updated_at=$2 WHERE reservation_id=$1 AND status='ADMITTED'`, request.Lease.ID, request.Now); err != nil {
				return scheduler.CircuitState{}, err
			}
			circuit, err = recordScheduleOutcome(ctx, tx, request.Lease.ID, tenant, environment, scheduler.ScheduleOutcomeUnknown, request.Circuit, request.Now)
			if err != nil {
				return scheduler.CircuitState{}, err
			}
			if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "SCHEDULE_UNKNOWN_RETAINED", "fence": fence}); err != nil {
				return scheduler.CircuitState{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return scheduler.CircuitState{}, err
		}
		return circuit, nil
	}
	if status == "RESERVED" {
		return scheduler.CircuitState{}, ErrScheduleLeaseConflict
	}
	if admissionCount == 0 {
		return scheduler.CircuitState{}, ErrScheduleLeaseConflict
	}
	if status != "ADMITTED" && status != "UNKNOWN" {
		return scheduler.CircuitState{}, ErrScheduleUnresolved
	}
	if !request.Now.Before(request.Lease.ExpiresAt) && status != "UNKNOWN" && status != string(request.Outcome) {
		return scheduler.CircuitState{}, ErrScheduleLeaseConflict
	}
	finalStatus := string(request.Outcome)
	if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_reservations SET status=$2,updated_at=$3 WHERE id=$1`, request.Lease.ID, finalStatus, request.Now); err != nil {
		return scheduler.CircuitState{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_admissions SET status=$2,updated_at=$3 WHERE reservation_id=$1 AND status IN ('ADMITTED','UNKNOWN')`, request.Lease.ID, finalStatus, request.Now); err != nil {
		return scheduler.CircuitState{}, err
	}
	if err := releaseScheduleScopes(ctx, tx, request.Lease.ID); err != nil {
		return scheduler.CircuitState{}, err
	}
	circuit, err = recordScheduleOutcome(ctx, tx, request.Lease.ID, tenant, environment, request.Outcome, request.Circuit, request.Now)
	if err != nil {
		return scheduler.CircuitState{}, err
	}
	if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "SCHEDULE_COMPLETED", "outcome": request.Outcome, "fence": fence}); err != nil {
		return scheduler.CircuitState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return scheduler.CircuitState{}, err
	}
	return circuit, nil
}

func (s *Store) QuarantineSchedule(ctx context.Context, request scheduler.QuarantineRequest) error {
	if request.ReservationID == "" || request.RunID == "" || request.Step.ID == "" || request.OwnerToken == "" || request.Now.IsZero() {
		return errors.New("invalid schedule quarantine request")
	}
	constraints, err := normalizeConstraints(request.Constraints)
	if err != nil {
		return err
	}
	request.Step = normalizeScheduleStep(request.Step)
	stepPayload, err := json.Marshal(request.Step)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id, status, tenant, environment string
	var payload []byte
	var fence int64
	err = tx.QueryRow(ctx, `SELECT id,status,fence,tenant_id,environment,step_payload FROM workflow_schedule_reservations WHERE run_id=$1 AND step_id=$2 FOR UPDATE`, request.RunID, request.Step.ID).Scan(&id, &status, &fence, &tenant, &environment, &payload)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if request.Step.Environment == "production" {
		if _, err := lockWorkflowCircuit(ctx, tx, coordinationTenant(request.Step.Tenant), request.Step.Environment, request.Circuit, request.Now); err != nil {
			return err
		}
	}
	if err == nil {
		if tenant != coordinationTenant(request.Step.Tenant) || environment != request.Step.Environment || !sameScheduleStep(payload, request.Step) {
			return ErrScheduleConfiguration
		}
		if status == "UNKNOWN" {
			if err := verifyScheduleScopes(ctx, tx, id, constraints); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if status != "RESERVED" && status != "ADMITTED" && status != "SUCCEEDED" && status != "FAILED" {
			return fmt.Errorf("cannot quarantine schedule status %s", status)
		}
		if err := verifyScheduleScopes(ctx, tx, id, constraints); err != nil {
			return err
		}
		if status == "SUCCEEDED" || status == "FAILED" {
			return tx.Commit(ctx)
		}
		if status == "RESERVED" || status == "ADMITTED" {
			if status == "ADMITTED" {
				var activeAdmissions int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_schedule_admissions WHERE reservation_id=$1 AND status='ADMITTED'`, id).Scan(&activeAdmissions); err != nil {
					return err
				}
				if activeAdmissions == 0 {
					return ErrScheduleUnresolved
				}
			}
			fence++
			if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_reservations SET owner_token=$2,fence=$3,status='UNKNOWN',lease_expires_at=$4,updated_at=$4 WHERE id=$1`, id, request.OwnerToken, fence, request.Now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_admissions SET status='UNKNOWN',updated_at=$2 WHERE reservation_id=$1 AND status='ADMITTED'`, id, request.Now); err != nil {
				return err
			}
		} else if err := reactivateScheduleScopes(ctx, tx, id, constraints); err != nil {
			return err
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		if err := lockScheduleCounters(ctx, tx, constraints); err != nil {
			return err
		}
		if err := ensureScheduleCountersLocked(ctx, tx, constraints); err != nil {
			return err
		}
		if err := ensureScheduleCountersLocked(ctx, tx, constraints); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_reservations
(id,run_id,step_id,tenant_id,environment,step_payload,owner_token,fence,status,lease_expires_at,created_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,1,'UNKNOWN',$8,$8,$8)`, request.ReservationID, request.RunID, request.Step.ID, coordinationTenant(request.Step.Tenant), request.Step.Environment, stepPayload, request.OwnerToken, request.Now); err != nil {
			return err
		}
		if err := activateScheduleScopes(ctx, tx, request.ReservationID, constraints); err != nil {
			return err
		}
		id = request.ReservationID
		fence = 1
	} else {
		return err
	}
	if request.Step.Environment == "production" {
		if _, err := recordScheduleOutcome(ctx, tx, id, coordinationTenant(request.Step.Tenant), request.Step.Environment, scheduler.ScheduleOutcomeUnknown, request.Circuit, request.Now); err != nil {
			return err
		}
	}
	if err := insertCoordinatorAudit(ctx, tx, request.Audit, map[string]any{"reason": "LEGACY_SCHEDULE_QUARANTINED", "fence": fence, "operation_key": request.OperationKey}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetCircuitState(ctx context.Context, tenant string) (scheduler.CircuitState, error) {
	var samplesJSON []byte
	var open bool
	err := s.pool.QueryRow(ctx, `SELECT samples,is_open FROM workflow_circuit_breakers WHERE tenant_id=$1 AND environment='production'`, coordinationTenant(tenant)).Scan(&samplesJSON, &open)
	if errors.Is(err, pgx.ErrNoRows) {
		return scheduler.CircuitState{}, nil
	}
	if err != nil {
		return scheduler.CircuitState{}, err
	}
	return circuitState(samplesJSON, open)
}

func normalizeConstraints(constraints []scheduler.Constraint) ([]scheduler.Constraint, error) {
	if len(constraints) == 0 {
		return nil, errors.New("schedule constraints are required")
	}
	result := append([]scheduler.Constraint(nil), constraints...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Dimension == result[j].Dimension {
			return result[i].Key < result[j].Key
		}
		return result[i].Dimension < result[j].Dimension
	})
	for index, constraint := range result {
		if constraint.Dimension == "" || constraint.Key == "" || constraint.Limit < 1 {
			return nil, errors.New("invalid schedule constraint")
		}
		if index > 0 && result[index-1].Dimension == constraint.Dimension && result[index-1].Key == constraint.Key {
			return nil, errors.New("duplicate schedule constraint")
		}
	}
	return result, nil
}

func normalizeScheduleStep(step scheduler.Step) scheduler.Step {
	step.FailureDomains = append([]string(nil), step.FailureDomains...)
	step.Dependencies = append([]string(nil), step.Dependencies...)
	sort.Strings(step.FailureDomains)
	sort.Strings(step.Dependencies)
	return step
}

func sameScheduleStep(payload []byte, expected scheduler.Step) bool {
	var existing scheduler.Step
	if json.Unmarshal(payload, &existing) != nil {
		return false
	}
	return reflect.DeepEqual(normalizeScheduleStep(existing), normalizeScheduleStep(expected))
}

func verifyScheduleScopes(ctx context.Context, tx pgx.Tx, reservationID string, expected []scheduler.Constraint) error {
	rows, err := tx.Query(ctx, `SELECT s.dimension,s.scope_key,c.limit_value
FROM workflow_schedule_scopes s JOIN workflow_schedule_counters c USING (dimension,scope_key)
WHERE s.reservation_id=$1 ORDER BY s.dimension,s.scope_key`, reservationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := make([]scheduler.Constraint, 0, len(expected))
	for rows.Next() {
		var constraint scheduler.Constraint
		if err := rows.Scan(&constraint.Dimension, &constraint.Key, &constraint.Limit); err != nil {
			return err
		}
		actual = append(actual, constraint)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return ErrScheduleConfiguration
	}
	return nil
}

func lockAndCheckScheduleCounters(ctx context.Context, tx pgx.Tx, constraints []scheduler.Constraint) ([]scheduler.Blocked, error) {
	blocked := make([]scheduler.Blocked, 0)
	for _, constraint := range constraints {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_counters(dimension,scope_key,limit_value,active_count)
VALUES ($1,$2,$3,0) ON CONFLICT DO NOTHING`, constraint.Dimension, constraint.Key, constraint.Limit); err != nil {
			return nil, err
		}
		var limit, active int
		if err := tx.QueryRow(ctx, `SELECT limit_value,active_count FROM workflow_schedule_counters WHERE dimension=$1 AND scope_key=$2 FOR UPDATE`, constraint.Dimension, constraint.Key).Scan(&limit, &active); err != nil {
			return nil, err
		}
		if limit != constraint.Limit {
			return nil, ErrScheduleConfiguration
		}
		if active >= limit {
			blocked = append(blocked, scheduler.Blocked{Dimension: constraint.Dimension, Key: constraint.Key, Limit: limit})
		}
	}
	return blocked, nil
}

func lockScheduleCounters(ctx context.Context, tx pgx.Tx, constraints []scheduler.Constraint) error {
	for _, constraint := range constraints {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_counters(dimension,scope_key,limit_value,active_count)
VALUES ($1,$2,$3,0) ON CONFLICT DO NOTHING`, constraint.Dimension, constraint.Key, constraint.Limit); err != nil {
			return err
		}
		var limit int
		if err := tx.QueryRow(ctx, `SELECT limit_value FROM workflow_schedule_counters WHERE dimension=$1 AND scope_key=$2 FOR UPDATE`, constraint.Dimension, constraint.Key).Scan(&limit); err != nil {
			return err
		}
		if limit != constraint.Limit {
			return ErrScheduleConfiguration
		}
	}
	return nil
}

func ensureScheduleCountersLocked(ctx context.Context, tx pgx.Tx, constraints []scheduler.Constraint) error {
	for _, constraint := range constraints {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_counters(dimension,scope_key,limit_value,active_count) VALUES ($1,$2,$3,0) ON CONFLICT DO NOTHING`, constraint.Dimension, constraint.Key, constraint.Limit); err != nil {
			return err
		}
		var active, limit int
		if err := tx.QueryRow(ctx, `SELECT active_count,limit_value FROM workflow_schedule_counters WHERE dimension=$1 AND scope_key=$2 FOR UPDATE`, constraint.Dimension, constraint.Key).Scan(&active, &limit); err != nil {
			return err
		}
		if limit != constraint.Limit {
			return ErrScheduleConfiguration
		}
		if active >= limit {
			return ErrScheduleUnresolved
		}
	}
	return nil
}

func activateScheduleScopes(ctx context.Context, tx pgx.Tx, reservationID string, constraints []scheduler.Constraint) error {
	for _, constraint := range constraints {
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_counters(dimension,scope_key,limit_value,active_count)
VALUES ($1,$2,$3,0) ON CONFLICT DO NOTHING`, constraint.Dimension, constraint.Key, constraint.Limit); err != nil {
			return err
		}
		var active bool
		err := tx.QueryRow(ctx, `SELECT active FROM workflow_schedule_scopes WHERE reservation_id=$1 AND dimension=$2 AND scope_key=$3 FOR UPDATE`, reservationID, constraint.Dimension, constraint.Key).Scan(&active)
		if err == nil {
			if active {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_scopes SET active=true WHERE reservation_id=$1 AND dimension=$2 AND scope_key=$3`, reservationID, constraint.Dimension, constraint.Key); err != nil {
				return err
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_scopes(reservation_id,dimension,scope_key,active) VALUES ($1,$2,$3,true)`, reservationID, constraint.Dimension, constraint.Key); err != nil {
				return err
			}
		} else {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_counters SET active_count=active_count+1 WHERE dimension=$1 AND scope_key=$2`, constraint.Dimension, constraint.Key); err != nil {
			return err
		}
	}
	return nil
}

func reactivateScheduleScopes(ctx context.Context, tx pgx.Tx, reservationID string, expected []scheduler.Constraint) error {
	rows, err := tx.Query(ctx, `SELECT dimension,scope_key,active FROM workflow_schedule_scopes WHERE reservation_id=$1 ORDER BY dimension,scope_key FOR UPDATE`, reservationID)
	if err != nil {
		return err
	}
	var scopes []scheduler.Constraint
	var inactive []scheduler.Constraint
	for rows.Next() {
		var scope scheduler.Constraint
		var active bool
		if err := rows.Scan(&scope.Dimension, &scope.Key, &active); err != nil {
			rows.Close()
			return err
		}
		scopes = append(scopes, scope)
		if !active {
			inactive = append(inactive, scope)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(scopes) != len(expected) {
		return ErrScheduleConfiguration
	}
	for index, scope := range scopes {
		if scope.Dimension != expected[index].Dimension || scope.Key != expected[index].Key {
			return ErrScheduleConfiguration
		}
	}
	for _, scope := range inactive {
		var active int
		if err := tx.QueryRow(ctx, `SELECT active_count FROM workflow_schedule_counters WHERE dimension=$1 AND scope_key=$2 FOR UPDATE`, scope.Dimension, scope.Key).Scan(&active); err != nil {
			return err
		}
		if active >= expectedLimit(expected, scope) {
			return ErrScheduleConfiguration
		}
		var configuredLimit int
		if err := tx.QueryRow(ctx, `SELECT limit_value FROM workflow_schedule_counters WHERE dimension=$1 AND scope_key=$2`, scope.Dimension, scope.Key).Scan(&configuredLimit); err != nil {
			return err
		}
		if configuredLimit != expectedLimit(expected, scope) {
			return ErrScheduleConfiguration
		}
		if active >= configuredLimit {
			return ErrScheduleUnresolved
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_counters SET active_count=active_count+1 WHERE dimension=$1 AND scope_key=$2`, scope.Dimension, scope.Key); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_scopes SET active=true WHERE reservation_id=$1 AND dimension=$2 AND scope_key=$3`, reservationID, scope.Dimension, scope.Key); err != nil {
			return err
		}
	}
	return nil
}

func expectedLimit(constraints []scheduler.Constraint, wanted scheduler.Constraint) int {
	for _, constraint := range constraints {
		if constraint.Dimension == wanted.Dimension && constraint.Key == wanted.Key {
			return constraint.Limit
		}
	}
	return 0
}

func releaseScheduleScopes(ctx context.Context, tx pgx.Tx, reservationID string) error {
	rows, err := tx.Query(ctx, `SELECT dimension,scope_key FROM workflow_schedule_scopes WHERE reservation_id=$1 AND active ORDER BY dimension,scope_key FOR UPDATE`, reservationID)
	if err != nil {
		return err
	}
	var scopes []scheduler.Constraint
	for rows.Next() {
		var scope scheduler.Constraint
		if err := rows.Scan(&scope.Dimension, &scope.Key); err != nil {
			rows.Close()
			return err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, scope := range scopes {
		var active int
		if err := tx.QueryRow(ctx, `SELECT active_count FROM workflow_schedule_counters WHERE dimension=$1 AND scope_key=$2 FOR UPDATE`, scope.Dimension, scope.Key).Scan(&active); err != nil {
			return err
		}
		if active < 1 {
			return errors.New("schedule coordinator counter underflow")
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_counters SET active_count=active_count-1 WHERE dimension=$1 AND scope_key=$2`, scope.Dimension, scope.Key); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_scopes SET active=false WHERE reservation_id=$1 AND dimension=$2 AND scope_key=$3`, reservationID, scope.Dimension, scope.Key); err != nil {
			return err
		}
	}
	return nil
}

func lockWorkflowCircuit(ctx context.Context, tx pgx.Tx, tenant, environment string, policy scheduler.CircuitPolicy, now time.Time) (scheduler.CircuitState, error) {
	if _, err := scheduler.NewCircuitBreaker(policy); err != nil {
		return scheduler.CircuitState{}, err
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return scheduler.CircuitState{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_circuit_breakers(tenant_id,environment,policy,samples,is_open,updated_at)
VALUES ($1,$2,$3,'[]'::jsonb,false,$4) ON CONFLICT DO NOTHING`, tenant, environment, policyJSON, now); err != nil {
		return scheduler.CircuitState{}, err
	}
	var storedPolicy, samples []byte
	var open bool
	if err := tx.QueryRow(ctx, `SELECT policy,samples,is_open FROM workflow_circuit_breakers WHERE tenant_id=$1 AND environment=$2 FOR UPDATE`, tenant, environment).Scan(&storedPolicy, &samples, &open); err != nil {
		return scheduler.CircuitState{}, err
	}
	var actual scheduler.CircuitPolicy
	if err := json.Unmarshal(storedPolicy, &actual); err != nil {
		return scheduler.CircuitState{}, err
	}
	if !reflect.DeepEqual(actual, policy) {
		return scheduler.CircuitState{}, ErrScheduleConfiguration
	}
	return circuitState(samples, open)
}

func recordScheduleOutcome(ctx context.Context, tx pgx.Tx, reservationID, tenant, environment string, outcome scheduler.ScheduleOutcome, policy scheduler.CircuitPolicy, now time.Time) (scheduler.CircuitState, error) {
	final := any(nil)
	resolved := any(nil)
	if outcome != scheduler.ScheduleOutcomeUnknown {
		final = string(outcome)
		resolved = now
	}
	result, err := tx.Exec(ctx, `INSERT INTO workflow_schedule_outcomes(reservation_id,first_outcome,final_outcome,recorded_at,resolved_at)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (reservation_id) DO NOTHING`, reservationID, outcome, final, now, resolved)
	if err != nil {
		return scheduler.CircuitState{}, err
	}
	if result.RowsAffected() == 0 {
		if outcome != scheduler.ScheduleOutcomeUnknown {
			if _, err := tx.Exec(ctx, `UPDATE workflow_schedule_outcomes SET final_outcome=COALESCE(final_outcome,$2),resolved_at=COALESCE(resolved_at,$3) WHERE reservation_id=$1`, reservationID, string(outcome), now); err != nil {
				return scheduler.CircuitState{}, err
			}
		}
		if environment == "production" {
			return readWorkflowCircuit(ctx, tx, tenant, environment, true)
		}
		return scheduler.CircuitState{}, nil
	}
	if environment != "production" {
		return scheduler.CircuitState{}, nil
	}
	state, err := lockWorkflowCircuit(ctx, tx, tenant, environment, policy, now)
	if err != nil {
		return scheduler.CircuitState{}, err
	}
	var samplesJSON []byte
	var open bool
	if err := tx.QueryRow(ctx, `SELECT samples,is_open FROM workflow_circuit_breakers WHERE tenant_id=$1 AND environment=$2 FOR UPDATE`, tenant, environment).Scan(&samplesJSON, &open); err != nil {
		return scheduler.CircuitState{}, err
	}
	var samples []bool
	if err := json.Unmarshal(samplesJSON, &samples); err != nil {
		return scheduler.CircuitState{}, err
	}
	samples = append(samples, outcome != scheduler.ScheduleOutcomeSucceeded)
	if len(samples) > policy.WindowSize {
		samples = append([]bool(nil), samples[len(samples)-policy.WindowSize:]...)
	}
	failures := 0
	for _, failed := range samples {
		if failed {
			failures++
		}
	}
	if len(samples) >= policy.MinimumSamples && float64(failures)/float64(len(samples)) >= policy.ErrorRate {
		open = true
	}
	encoded, err := json.Marshal(samples)
	if err != nil {
		return scheduler.CircuitState{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_circuit_breakers SET samples=$3,is_open=$4,updated_at=$5 WHERE tenant_id=$1 AND environment=$2`, tenant, environment, encoded, open, now); err != nil {
		return scheduler.CircuitState{}, err
	}
	state.Samples = len(samples)
	state.Failures = failures
	state.ErrorRate = float64(failures) / float64(len(samples))
	state.Open = open
	return state, nil
}

func readWorkflowCircuit(ctx context.Context, tx pgx.Tx, tenant, environment string, lock bool) (scheduler.CircuitState, error) {
	query := `SELECT samples,is_open FROM workflow_circuit_breakers WHERE tenant_id=$1 AND environment=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	var samples []byte
	var open bool
	if err := tx.QueryRow(ctx, query, tenant, environment).Scan(&samples, &open); err != nil {
		return scheduler.CircuitState{}, err
	}
	return circuitState(samples, open)
}

func circuitState(samplesJSON []byte, open bool) (scheduler.CircuitState, error) {
	var samples []bool
	if len(samplesJSON) > 0 {
		if err := json.Unmarshal(samplesJSON, &samples); err != nil {
			return scheduler.CircuitState{}, err
		}
	}
	failures := 0
	for _, failed := range samples {
		if failed {
			failures++
		}
	}
	rate := 0.0
	if len(samples) > 0 {
		rate = float64(failures) / float64(len(samples))
	}
	return scheduler.CircuitState{Open: open, Samples: len(samples), Failures: failures, ErrorRate: rate}, nil
}

func insertCoordinatorAudit(ctx context.Context, tx pgx.Tx, event domain.AuditEvent, extra map[string]any) error {
	if event.ID == "" {
		return errors.New("coordinator audit event ID is required")
	}
	details := make(map[string]any, len(event.Details)+len(extra))
	for key, value := range event.Details {
		details[key] = value
	}
	for key, value := range extra {
		details[key] = value
	}
	event.Details = details
	return insertAudit(ctx, tx, event)
}

func coordinationTenant(tenant string) string {
	if tenant == "" {
		return "default"
	}
	return tenant
}

func reservationLockKey(runID, stepID string) string {
	return runID + "/" + stepID
}

var _ scheduler.DurableCoordinator = (*Store)(nil)
