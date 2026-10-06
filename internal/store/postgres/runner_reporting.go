package postgres

import (
	"context"
	"encoding/json"
	"themisy/internal/domain"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

func (s *Store) UnreportedRunnerActions(ctx context.Context, tenant, group string, limit int) ([]store.RunnerActionRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+runnerActionColumns+` FROM runner_journal WHERE tenant_id=$1 AND runner_group=$2 AND reported_version<state_version AND status IN ('SUCCEEDED','UNKNOWN','FAILED','REJECTED') AND result->>'completed_at' IS NOT NULL ORDER BY updated_at LIMIT $3`, tenant, group, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.RunnerActionRecord{}
	for rows.Next() {
		r, err := scanRunnerAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) MarkRunnerReported(ctx context.Context, r store.RunnerActionRecord) error {
	tag, err := s.pool.Exec(ctx, `UPDATE runner_journal SET reported_version=$4 WHERE tenant_id=$1 AND runner_group=$2 AND nonce=$3 AND state_version=$4`, r.TenantID, r.RunnerGroup, r.Nonce, r.StateVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return store.ErrConflict
	}
	return nil
}

func (s *Store) ReconcileGrantDispatch(ctx context.Context, id string, expected int64, result protocol.Result, audit domain.AuditEvent) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	record, err := scanGrantDispatch(tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM action_grants WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if record.Grant.RunID != result.RunID || record.Grant.StepID != result.StepID || result.GrantID != id {
		return store.ErrConflict
	}
	if record.Result == result {
		return nil
	}
	if record.StateVersion != expected || record.Status == store.GrantDispatchSucceeded || record.Status == store.GrantDispatchRejected {
		return store.ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE action_grants SET status=$2,result=$3,completed_at=$4,updated_at=$4,state_version=state_version+1 WHERE id=$1`, id, grantDispatchStatus(result.Status), payload, audit.Timestamp)
	if err != nil {
		return err
	}
	if err = insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
