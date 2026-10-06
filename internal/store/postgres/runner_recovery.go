package postgres

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"themisy/internal/domain"
	"themisy/internal/store"
	"time"
)

const runnerActionColumns = `grant_id,run_id,step_id,tenant_id,runner_group,nonce,idempotency_key,request_hash,target,action,status,result,state_version,created_at,updated_at,runner_id,reconcile_owner,reconcile_until,plan_revision,plan_hash`

func (s *Store) ClaimRunnerActions(ctx context.Context, tenant, group, owner string, now, until time.Time, limit int) ([]store.RunnerActionRecord, error) {
	if owner == "" || !until.After(now) {
		return nil, store.ErrConflict
	}
	if limit <= 0 {
		limit = 100
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT `+runnerActionColumns+` FROM runner_journal
 WHERE tenant_id=$1 AND runner_group=$2 AND status IN ('RESERVED','DISPATCHED','UNKNOWN')
 AND (reconcile_until IS NULL OR reconcile_until<=$3)
 ORDER BY updated_at LIMIT $4 FOR UPDATE SKIP LOCKED`, tenant, group, now, limit)
	if err != nil {
		return nil, err
	}
	records := []store.RunnerActionRecord{}
	for rows.Next() {
		r, err := scanRunnerAction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range records {
		r := &records[i]
		// Claiming RESERVED fences its original writer before dispatch. The record
		// remains unresolved; a recovery worker never inherits permission to write.
		_, err := tx.Exec(ctx, `UPDATE runner_journal SET reconcile_owner=$4,reconcile_until=$5,state_version=state_version+1 WHERE tenant_id=$1 AND runner_group=$2 AND nonce=$3`, tenant, group, r.Nonce, owner, until)
		if err != nil {
			return nil, err
		}
		r.StateVersion++
		r.ReconcileOwner = owner
		r.ReconcileUntil = until
		event := domain.AuditEvent{ID: fmt.Sprintf("%s/reconcile-claim/%d", r.GrantID, r.StateVersion), CorrelationID: r.GrantID, ActorType: "runner", ActorID: owner, Action: "runner.reconcile.claim", ResourceType: "runner_action", ResourceID: r.GrantID, Result: "CLAIMED", Timestamp: now}
		if err := insertAudit(ctx, tx, event); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return records, nil
}
