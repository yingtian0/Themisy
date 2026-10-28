package postgres

import (
	"context"
	"themisy/internal/store"
)

func (s *Store) AcceptContextRevision(ctx context.Context, tenant, issuer string, revision int64, hash string) error {
	if tenant == "" || issuer == "" || revision < 1 || hash == "" {
		return store.ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO context_revision_fences(tenant_id,issuer,revision,context_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, tenant, issuer, revision, hash); err != nil {
		return err
	}
	var old int64
	var oldHash string
	if err := tx.QueryRow(ctx, `SELECT revision,context_hash FROM context_revision_fences WHERE tenant_id=$1 AND issuer=$2 FOR UPDATE`, tenant, issuer).Scan(&old, &oldHash); err != nil {
		return err
	}
	if revision < old || (revision == old && hash != oldHash) {
		return store.ErrConflict
	}
	if revision > old {
		if _, err := tx.Exec(ctx, `UPDATE context_revision_fences SET revision=$3,context_hash=$4 WHERE tenant_id=$1 AND issuer=$2`, tenant, issuer, revision, hash); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
