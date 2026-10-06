package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"themisy/internal/domain"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

func (s *Store) PinManifest(ctx context.Context, m protocol.PlanManifest) error {
	hash, err := protocol.ManifestHash(m)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO plan_revisions VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, m.TenantID, m.RunnerGroup, m.RunID, m.Revision, m.PlanHash)
	if err != nil {
		return err
	}
	var revision int64
	var plan string
	if err = tx.QueryRow(ctx, `SELECT revision,plan_hash FROM plan_revisions WHERE tenant_id=$1 AND runner_group=$2 AND run_id=$3 FOR UPDATE`, m.TenantID, m.RunnerGroup, m.RunID).Scan(&revision, &plan); err != nil {
		return err
	}
	if m.Revision < revision || (m.Revision == revision && m.PlanHash != plan) {
		return store.ErrConflict
	}
	if m.Revision > revision {
		if _, err = tx.Exec(ctx, `UPDATE plan_revisions SET revision=$4,plan_hash=$5 WHERE tenant_id=$1 AND runner_group=$2 AND run_id=$3`, m.TenantID, m.RunnerGroup, m.RunID, m.Revision, m.PlanHash); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO plan_manifests VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, m.TenantID, m.RunnerGroup, m.RunID, m.StepID, m.Action.Capability, m.Revision, hash, payload)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var existing string
		if err = tx.QueryRow(ctx, `SELECT content_hash FROM plan_manifests WHERE tenant_id=$1 AND runner_group=$2 AND run_id=$3 AND step_id=$4 AND capability=$5 AND revision=$6`, m.TenantID, m.RunnerGroup, m.RunID, m.StepID, m.Action.Capability, m.Revision).Scan(&existing); err != nil {
			return err
		}
		if existing != hash {
			return store.ErrConflict
		}
	} else {
		audit := domain.AuditEvent{ID: fmt.Sprintf("manifest/%s/%s/%s/%d/%s", m.TenantID, m.RunnerGroup, m.RunID, m.Revision, hash), CorrelationID: m.RunID, ActorType: "manifest", ActorID: m.Issuer, Action: "runner.manifest.pin", ResourceType: "plan_manifest", ResourceID: hash, Result: "PINNED", Timestamp: m.IssuedAt}
		if err = insertAudit(ctx, tx, audit); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) GetManifest(ctx context.Context, tenant, group, run, step string, capability protocol.Capability) (protocol.PlanManifest, error) {
	var payload []byte
	err := s.pool.QueryRow(ctx, `SELECT m.payload FROM plan_manifests m JOIN plan_revisions r USING(tenant_id,runner_group,run_id,revision) WHERE m.tenant_id=$1 AND m.runner_group=$2 AND m.run_id=$3 AND m.step_id=$4 AND m.capability=$5`, tenant, group, run, step, capability).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return protocol.PlanManifest{}, store.ErrNotFound
	}
	if err != nil {
		return protocol.PlanManifest{}, err
	}
	var m protocol.PlanManifest
	err = json.Unmarshal(payload, &m)
	return m, err
}
