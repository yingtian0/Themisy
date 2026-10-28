package postgres

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"themisy/internal/store"
)

// AcceptIdentityDirectory persists the rollback fence independently in the
// Control Plane or customer Runner schema selected by this Store's pool.
func (s *Store) AcceptIdentityDirectory(ctx context.Context, tenant, issuer string, revision int64, hash string) error {
	if tenant == "" || issuer == "" || revision < 1 || hash == "" {
		return store.ErrConflict
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO identity_directory_versions(tenant_id,issuer,revision,bundle_hash) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, tenant, issuer, revision, hash); err != nil {
		return err
	}
	var old int64
	var oldHash string
	if err := tx.QueryRow(ctx, `SELECT revision,bundle_hash FROM identity_directory_versions WHERE tenant_id=$1 AND issuer=$2 FOR UPDATE`, tenant, issuer).Scan(&old, &oldHash); err != nil {
		return err
	}
	if revision < old || (revision == old && oldHash != hash) {
		return fmt.Errorf("identity directory revision: %w", store.ErrConflict)
	}
	if revision > old {
		if _, err := tx.Exec(ctx, `UPDATE identity_directory_versions SET revision=$3,bundle_hash=$4 WHERE tenant_id=$1 AND issuer=$2`, tenant, issuer, revision, hash); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
