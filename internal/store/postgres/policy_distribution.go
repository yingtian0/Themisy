package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"themisy/internal/domain"
	policy "themisy/internal/policymodel"
	"themisy/internal/store"
)

func (s *Store) RegisterPolicyBundle(ctx context.Context, tenant string, b policy.Bundle, audit domain.AuditEvent) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	inserted, err := tx.Exec(ctx, `INSERT INTO scoped_policy_bundles(tenant_id,bundle_hash,issuer,version,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, tenant, b.Hash, b.Issuer, b.Version, raw)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() == 0 {
		var identical bool
		if err := tx.QueryRow(ctx, `SELECT payload=$3::jsonb FROM scoped_policy_bundles WHERE tenant_id=$1 AND bundle_hash=$2`, tenant, b.Hash, raw).Scan(&identical); err != nil || !identical {
			return policy.ErrAssignmentConflict
		}
		return tx.Commit(ctx)
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) GetPolicyBundle(ctx context.Context, tenant, hash string) (policy.Bundle, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT payload FROM scoped_policy_bundles WHERE tenant_id=$1 AND bundle_hash=$2`, tenant, hash).Scan(&raw)
	if err != nil {
		return policy.Bundle{}, err
	}
	var b policy.Bundle
	err = json.Unmarshal(raw, &b)
	return b, err
}

const assignmentColumns = `tenant_id,runner_group,revision,bundle_hash,previous_hash,updated_at`

func scanPolicyAssignment(row pgx.Row) (policy.Assignment, error) {
	var a policy.Assignment
	err := row.Scan(&a.TenantID, &a.RunnerGroup, &a.Revision, &a.BundleHash, &a.PreviousHash, &a.UpdatedAt)
	return a, err
}
func (s *Store) GetPolicyAssignment(ctx context.Context, tenant, group string) (policy.Assignment, error) {
	return scanPolicyAssignment(s.pool.QueryRow(ctx, `SELECT `+assignmentColumns+` FROM policy_assignments WHERE tenant_id=$1 AND runner_group=$2`, tenant, group))
}
func (s *Store) SetPolicyAssignment(ctx context.Context, a policy.Assignment, expected int64, audit domain.AuditEvent) (policy.Assignment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return policy.Assignment{}, err
	}
	defer tx.Rollback(ctx)
	var row pgx.Row
	if expected == 0 {
		row = tx.QueryRow(ctx, `INSERT INTO policy_assignments(tenant_id,runner_group,revision,bundle_hash,updated_at) VALUES($1,$2,1,$3,$4) ON CONFLICT DO NOTHING RETURNING `+assignmentColumns, a.TenantID, a.RunnerGroup, a.BundleHash, a.UpdatedAt)
	} else {
		row = tx.QueryRow(ctx, `UPDATE policy_assignments SET previous_hash=bundle_hash,bundle_hash=$3,revision=revision+1,updated_at=$4 WHERE tenant_id=$1 AND runner_group=$2 AND revision=$5 AND bundle_hash<>$3 RETURNING `+assignmentColumns, a.TenantID, a.RunnerGroup, a.BundleHash, a.UpdatedAt, expected)
	}
	updated, err := scanPolicyAssignment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.Assignment{}, policy.ErrAssignmentConflict
	}
	if err != nil {
		return policy.Assignment{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return policy.Assignment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return policy.Assignment{}, err
	}
	return updated, nil
}
func (s *Store) AcknowledgePolicy(ctx context.Context, tenant, group string, ack policy.Acknowledgement, audit domain.AuditEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var revision int64
	var hash string
	if err := tx.QueryRow(ctx, `SELECT revision,bundle_hash FROM policy_assignments WHERE tenant_id=$1 AND runner_group=$2 FOR SHARE`, tenant, group).Scan(&revision, &hash); err != nil {
		return err
	}
	if revision != ack.Revision || hash != ack.BundleHash {
		return policy.ErrAssignmentConflict
	}
	result, err := tx.Exec(ctx, `INSERT INTO policy_acknowledgements(tenant_id,runner_group,runner_id,revision,bundle_hash,observed_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, tenant, group, ack.RunnerID, ack.Revision, ack.BundleHash, ack.ObservedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() > 0 {
		if err := insertAudit(ctx, tx, audit); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) PolicyAcknowledgements(ctx context.Context, tenant, group string, revision int64) ([]policy.Acknowledgement, error) {
	rows, err := s.pool.Query(ctx, `SELECT runner_id,revision,bundle_hash,observed_at FROM policy_acknowledgements WHERE tenant_id=$1 AND runner_group=$2 AND revision=$3 ORDER BY runner_id`, tenant, group, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []policy.Acknowledgement{}
	for rows.Next() {
		var a policy.Acknowledgement
		if err := rows.Scan(&a.RunnerID, &a.Revision, &a.BundleHash, &a.ObservedAt); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
func (s *Store) AcceptPolicyRevision(ctx context.Context, a policy.Assignment) error {
	if a.TenantID == "" || a.RunnerGroup == "" || a.Revision < 1 || a.BundleHash == "" {
		return store.ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO policy_revision_fences(tenant_id,runner_group,revision,bundle_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, a.TenantID, a.RunnerGroup, a.Revision, a.BundleHash); err != nil {
		return err
	}
	var revision int64
	var hash string
	if err := tx.QueryRow(ctx, `SELECT revision,bundle_hash FROM policy_revision_fences WHERE tenant_id=$1 AND runner_group=$2 FOR UPDATE`, a.TenantID, a.RunnerGroup).Scan(&revision, &hash); err != nil {
		return err
	}
	if a.Revision < revision || (a.Revision == revision && a.BundleHash != hash) {
		return policy.ErrAssignmentConflict
	}
	if a.Revision > revision {
		if _, err := tx.Exec(ctx, `UPDATE policy_revision_fences SET revision=$3,bundle_hash=$4 WHERE tenant_id=$1 AND runner_group=$2`, a.TenantID, a.RunnerGroup, a.Revision, a.BundleHash); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
