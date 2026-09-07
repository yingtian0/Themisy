package postgres

import (
	"context"
	"errors"
	"time"

	"themisy/internal/domain"
	"themisy/internal/store"

	"github.com/jackc/pgx/v5"
)

const runnerFleetColumns = `runner_id,tenant_id,runner_group,status,capacity,reported_capacity,in_flight,last_seen,state_version,COALESCE(controlled_by,''),controlled_at,COALESCE(frozen_by,''),frozen_at`

func (s *Store) HeartbeatRunner(ctx context.Context, heartbeat domain.RunnerInfo, audit domain.AuditEvent) (domain.RunnerInfo, error) {
	if heartbeat.ID == "" || heartbeat.TenantID == "" || heartbeat.Group == "" || heartbeat.ReportedCapacity < 0 || heartbeat.InFlight < 0 || heartbeat.InFlight > heartbeat.ReportedCapacity || heartbeat.LastSeen.IsZero() {
		return domain.RunnerInfo{}, store.ErrConflict
	}
	available := heartbeat.ReportedCapacity - heartbeat.InFlight
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RunnerInfo{}, err
	}
	defer tx.Rollback(ctx)
	value, err := scanRunner(tx.QueryRow(ctx, `
INSERT INTO runner_fleet (tenant_id,runner_id,runner_group,status,capacity,reported_capacity,in_flight,last_seen,state_version)
VALUES ($1,$2,$3,'READY',$4,$5,$6,$7,1)
ON CONFLICT (tenant_id,runner_id) DO UPDATE SET
  reported_capacity=EXCLUDED.reported_capacity,
  in_flight=EXCLUDED.in_flight,
  capacity=CASE WHEN runner_fleet.status='READY' THEN EXCLUDED.capacity ELSE 0 END,
  last_seen=EXCLUDED.last_seen,
  state_version=runner_fleet.state_version+1
WHERE runner_fleet.runner_group=EXCLUDED.runner_group
RETURNING `+runnerFleetColumns, heartbeat.TenantID, heartbeat.ID, heartbeat.Group, available, heartbeat.ReportedCapacity, heartbeat.InFlight, heartbeat.LastSeen))
	if errors.Is(err, store.ErrNotFound) {
		return domain.RunnerInfo{}, store.ErrConflict
	}
	if err != nil {
		return domain.RunnerInfo{}, err
	}
	if audit.ID != "" {
		if err := insertAudit(ctx, tx, audit); err != nil {
			return domain.RunnerInfo{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RunnerInfo{}, err
	}
	return value, nil
}

func (s *Store) GetRunner(ctx context.Context, tenantID, runnerID string) (domain.RunnerInfo, error) {
	return scanRunner(s.pool.QueryRow(ctx, `SELECT `+runnerFleetColumns+` FROM runner_fleet WHERE tenant_id=$1 AND runner_id=$2`, tenantID, runnerID))
}

func (s *Store) ListRunnerFleet(ctx context.Context, tenantID string) ([]domain.RunnerInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runnerFleetColumns+` FROM runner_fleet WHERE tenant_id=$1 ORDER BY runner_id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.RunnerInfo{}
	for rows.Next() {
		value, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) SetRunnerStatus(ctx context.Context, tenantID, runnerID string, status domain.RunnerStatus, actor string, now time.Time, audit domain.AuditEvent) (domain.RunnerInfo, error) {
	if !validRunnerStatus(status) || actor == "" {
		return domain.RunnerInfo{}, store.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RunnerInfo{}, err
	}
	defer tx.Rollback(ctx)
	value, err := scanRunner(tx.QueryRow(ctx, `
UPDATE runner_fleet SET
  status=$3,
  capacity=CASE WHEN $3='READY' THEN GREATEST(reported_capacity-in_flight,0) ELSE 0 END,
  controlled_by=$4,
  controlled_at=$5,
  frozen_by=CASE WHEN $3='FROZEN' THEN $4 ELSE NULL END,
  frozen_at=CASE WHEN $3='FROZEN' THEN $5 ELSE NULL END,
  state_version=state_version+1
WHERE tenant_id=$1 AND runner_id=$2
RETURNING `+runnerFleetColumns, tenantID, runnerID, status, actor, now))
	if err != nil {
		return domain.RunnerInfo{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return domain.RunnerInfo{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RunnerInfo{}, err
	}
	return value, nil
}

type runnerRow interface{ Scan(...any) error }

func scanRunner(row runnerRow) (domain.RunnerInfo, error) {
	var value domain.RunnerInfo
	var controlledAt, frozenAt *time.Time
	if err := row.Scan(&value.ID, &value.TenantID, &value.Group, &value.Status, &value.Capacity, &value.ReportedCapacity, &value.InFlight, &value.LastSeen, &value.StateVersion, &value.ControlledBy, &controlledAt, &value.FrozenBy, &frozenAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.RunnerInfo{}, store.ErrNotFound
		}
		return domain.RunnerInfo{}, err
	}
	value.ControlledAt, value.FrozenAt = controlledAt, frozenAt
	return value, nil
}

func validRunnerStatus(status domain.RunnerStatus) bool {
	return status == domain.RunnerReady || status == domain.RunnerDraining || status == domain.RunnerFrozen
}
