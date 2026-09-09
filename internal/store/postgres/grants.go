package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"themisy/internal/domain"
	"themisy/internal/store"
	"themisy/pkg/protocol"

	"github.com/jackc/pgx/v5"
)

const grantColumns = `id,run_id,tenant_id,runner_group,status,payload,result,COALESCE(leased_by,''),COALESCE(delivery_token,''),lease_expires_at,acknowledged_at,completed_at,COALESCE(outbox_id,''),state_version,created_at,updated_at`

func (s *Store) CreateGrantDispatch(ctx context.Context, record store.GrantDispatchRecord, audit domain.AuditEvent, outbox domain.OutboxEvent) (store.GrantDispatchRecord, bool, error) {
	payload, err := json.Marshal(record.Grant)
	if err != nil {
		return store.GrantDispatchRecord{}, false, err
	}
	result, err := json.Marshal(record.Result)
	if err != nil {
		return store.GrantDispatchRecord{}, false, err
	}
	record.Status, record.StateVersion, record.OutboxID = store.GrantDispatchPending, 1, outbox.ID
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.GrantDispatchRecord{}, false, err
	}
	defer tx.Rollback(ctx)
	row := tx.QueryRow(ctx, `
INSERT INTO action_grants (id,run_id,tenant_id,runner_group,issuer,idempotency_key,nonce,policy_hash,status,payload,result,outbox_id,state_version,created_at,updated_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT DO NOTHING
RETURNING `+grantColumns,
		record.Grant.GrantID, record.Grant.RunID, record.Grant.TenantID, record.Grant.RunnerGroup, record.Grant.Issuer,
		record.Grant.IdempotencyKey, record.Grant.Nonce, record.Grant.PolicyHash, record.Status, payload, result, record.OutboxID,
		record.StateVersion, record.CreatedAt, record.UpdatedAt, record.Grant.ExpiresAt)
	created, scanErr := scanGrantDispatch(row)
	if scanErr == nil {
		if err := insertEvents(ctx, tx, []domain.AuditEvent{audit}, []domain.OutboxEvent{outbox}); err != nil {
			return store.GrantDispatchRecord{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return store.GrantDispatchRecord{}, false, err
		}
		return created, true, nil
	}
	if !errors.Is(scanErr, store.ErrNotFound) {
		return store.GrantDispatchRecord{}, false, scanErr
	}
	existing, err := scanGrantDispatch(tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM action_grants WHERE tenant_id=$1 AND runner_group=$2 AND idempotency_key=$3`, record.Grant.TenantID, record.Grant.RunnerGroup, record.Grant.IdempotencyKey))
	if err != nil {
		return store.GrantDispatchRecord{}, false, err
	}
	return existing, false, nil
}

func (s *Store) ClaimGrantDispatch(ctx context.Context, tenantID, runnerGroup, runnerID, deliveryToken string, now, leaseExpiresAt time.Time) (store.GrantDispatchRecord, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.GrantDispatchRecord{}, err
	}
	defer tx.Rollback(ctx)
	record, err := scanGrantDispatch(tx.QueryRow(ctx, `
WITH candidate AS (
    SELECT id FROM action_grants
    WHERE tenant_id=$1 AND runner_group=$2 AND expires_at>$5
      AND status IN ('PENDING','LEASED','ACKED')
      AND (lease_expires_at IS NULL OR lease_expires_at<=$5 OR leased_by=$3)
    ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE action_grants AS g SET
    status='LEASED',
    leased_by=$3,
    delivery_token=CASE WHEN g.leased_by=$3 AND g.lease_expires_at>$5 THEN g.delivery_token ELSE $4 END,
    lease_expires_at=$6,
    state_version=g.state_version+1,
    updated_at=$5
FROM candidate WHERE g.id=candidate.id
RETURNING `+qualifiedGrantColumns("g"), tenantID, runnerGroup, runnerID, deliveryToken, now, leaseExpiresAt))
	if err != nil {
		return store.GrantDispatchRecord{}, err
	}
	if record.OutboxID != "" {
		_, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at=COALESCE(published_at,$2),attempts=CASE WHEN published_at IS NULL THEN attempts+1 ELSE attempts END WHERE id=$1`, record.OutboxID, now)
		if err != nil {
			return store.GrantDispatchRecord{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.GrantDispatchRecord{}, err
	}
	return record, nil
}

func (s *Store) AcknowledgeGrantDispatch(ctx context.Context, grantID, runnerID, token string, now, leaseExpiresAt time.Time) (store.GrantDispatchRecord, error) {
	record, err := scanGrantDispatch(s.pool.QueryRow(ctx, `
UPDATE action_grants SET status='ACKED',acknowledged_at=$4,lease_expires_at=$5,state_version=state_version+1,updated_at=$4
WHERE id=$1 AND leased_by=$2 AND delivery_token=$3 AND lease_expires_at>$4 AND status IN ('LEASED','ACKED')
RETURNING `+grantColumns, grantID, runnerID, token, now, leaseExpiresAt))
	if errors.Is(err, store.ErrNotFound) {
		return store.GrantDispatchRecord{}, store.ErrConflict
	}
	return record, err
}

func (s *Store) CompleteGrantDispatch(ctx context.Context, grantID, runnerID, token string, result protocol.Result, now time.Time, audit domain.AuditEvent) (store.GrantDispatchRecord, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return store.GrantDispatchRecord{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.GrantDispatchRecord{}, err
	}
	defer tx.Rollback(ctx)
	status := grantDispatchStatus(result.Status)
	record, err := scanGrantDispatch(tx.QueryRow(ctx, `
UPDATE action_grants SET status=$5,result=$6,completed_at=$7,consumed_at=$7,state_version=state_version+1,updated_at=$7
WHERE id=$1 AND leased_by=$2 AND delivery_token=$3 AND status IN ('LEASED','ACKED')
  AND run_id=$4
RETURNING `+grantColumns, grantID, runnerID, token, result.RunID, status, encoded, now))
	if errors.Is(err, store.ErrNotFound) {
		existing, getErr := scanGrantDispatch(tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM action_grants WHERE id=$1`, grantID))
		if getErr == nil && existing.RunnerID == runnerID && existing.DeliveryToken == token && existing.Result == result {
			return existing, nil
		}
		return store.GrantDispatchRecord{}, store.ErrConflict
	}
	if err != nil {
		return store.GrantDispatchRecord{}, err
	}
	if result.GrantID != record.Grant.GrantID || result.StepID != record.Grant.StepID {
		return store.GrantDispatchRecord{}, store.ErrConflict
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return store.GrantDispatchRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.GrantDispatchRecord{}, err
	}
	return record, nil
}

func (s *Store) GetGrantDispatch(ctx context.Context, grantID string) (store.GrantDispatchRecord, error) {
	return scanGrantDispatch(s.pool.QueryRow(ctx, `SELECT `+grantColumns+` FROM action_grants WHERE id=$1`, grantID))
}

type grantRow interface{ Scan(...any) error }

func scanGrantDispatch(row grantRow) (store.GrantDispatchRecord, error) {
	var record store.GrantDispatchRecord
	var payload, result []byte
	var leaseExpiresAt, acknowledgedAt, completedAt *time.Time
	err := row.Scan(&record.Grant.GrantID, &record.Grant.RunID, &record.Grant.TenantID, &record.Grant.RunnerGroup, &record.Status,
		&payload, &result, &record.RunnerID, &record.DeliveryToken, &leaseExpiresAt, &acknowledgedAt, &completedAt,
		&record.OutboxID, &record.StateVersion, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.GrantDispatchRecord{}, store.ErrNotFound
	}
	if err != nil {
		return store.GrantDispatchRecord{}, err
	}
	if err := json.Unmarshal(payload, &record.Grant); err != nil {
		return store.GrantDispatchRecord{}, err
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &record.Result); err != nil {
			return store.GrantDispatchRecord{}, err
		}
	}
	if leaseExpiresAt != nil {
		record.LeaseExpiresAt = *leaseExpiresAt
	}
	if acknowledgedAt != nil {
		record.AcknowledgedAt = *acknowledgedAt
	}
	if completedAt != nil {
		record.CompletedAt = *completedAt
	}
	return record, nil
}

func qualifiedGrantColumns(alias string) string {
	return alias + `.id,` + alias + `.run_id,` + alias + `.tenant_id,` + alias + `.runner_group,` + alias + `.status,` + alias + `.payload,` + alias + `.result,COALESCE(` + alias + `.leased_by,''),COALESCE(` + alias + `.delivery_token,''),` + alias + `.lease_expires_at,` + alias + `.acknowledged_at,` + alias + `.completed_at,COALESCE(` + alias + `.outbox_id,''),` + alias + `.state_version,` + alias + `.created_at,` + alias + `.updated_at`
}

func grantDispatchStatus(status protocol.ResultStatus) store.GrantDispatchStatus {
	switch status {
	case protocol.ResultSucceeded:
		return store.GrantDispatchSucceeded
	case protocol.ResultRejected:
		return store.GrantDispatchRejected
	default:
		return store.GrantDispatchUnknown
	}
}
