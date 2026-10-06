package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"themisy/internal/domain"
	"themisy/internal/store"
	rootmigrations "themisy/migrations"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres configuration: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func Migrate(databaseURL string, down bool) error {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{DatabaseName: config.ConnConfig.Database})
	if err != nil {
		return err
	}
	source, err := iofs.New(rootmigrations.FS, ".")
	if err != nil {
		return err
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return err
	}
	defer migrator.Close()
	if down {
		err = migrator.Down()
	} else {
		err = migrator.Up()
	}
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	return err
}

func (s *Store) CreateRun(run *domain.ReleaseRun) (*domain.ReleaseRun, bool, error) {
	return s.CreateRunAtomic(run, nil, nil)
}

func (s *Store) CreateRunAtomic(run *domain.ReleaseRun, audits []domain.AuditEvent, outbox []domain.OutboxEvent) (*domain.ReleaseRun, bool, error) {
	ctx := context.Background()
	stored := cloneRun(run)
	if stored.WorkflowID == "" {
		stored.WorkflowID = stored.ID
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, insertRunSQL,
		stored.ID, stored.RequestID, stored.WorkflowID, stored.TemporalRunID,
		stored.ReleaseVersion, stored.Environment, stored.RequestedBy, stored.Status,
		stored.StateVersion, payload, stored.CreatedAt, stored.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			existing, getErr := s.getRunByRequest(ctx, stored.RequestID)
			return existing, false, getErr
		}
		return nil, false, err
	}
	if err := syncChildren(ctx, tx, stored); err != nil {
		return nil, false, err
	}
	if err := insertEvents(ctx, tx, audits, appendProjectionEvent(outbox, stored)); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return cloneRun(stored), true, nil
}

func (s *Store) GetRun(id string) (*domain.ReleaseRun, error) {
	return scanRun(s.pool.QueryRow(context.Background(), selectRunSQL, id))
}

func (s *Store) SavePlan(plan domain.ReleasePlan) error {
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(context.Background(), `
INSERT INTO release_plans (id,plan_hash,environment,payload,created_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (id) DO UPDATE SET plan_hash=EXCLUDED.plan_hash,environment=EXCLUDED.environment,
payload=EXCLUDED.payload,created_at=EXCLUDED.created_at,expires_at=EXCLUDED.expires_at`,
		plan.ID, plan.PlanHash, plan.Environment, payload, plan.CreatedAt, plan.ExpiresAt)
	return err
}

func (s *Store) GetPlan(id string) (domain.ReleasePlan, error) {
	var payload []byte
	if err := s.pool.QueryRow(context.Background(), `SELECT payload FROM release_plans WHERE id=$1`, id).Scan(&payload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ReleasePlan{}, store.ErrNotFound
		}
		return domain.ReleasePlan{}, err
	}
	var plan domain.ReleasePlan
	if err := json.Unmarshal(payload, &plan); err != nil {
		return domain.ReleasePlan{}, err
	}
	return plan, nil
}

func (s *Store) ListRuns() ([]*domain.ReleaseRun, error) {
	rows, err := s.pool.Query(context.Background(), `SELECT payload FROM release_runs ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]*domain.ReleaseRun, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var run domain.ReleaseRun
		if err := json.Unmarshal(payload, &run); err != nil {
			return nil, err
		}
		runs = append(runs, &run)
	}
	return runs, rows.Err()
}

func (s *Store) getRunByRequest(ctx context.Context, requestID string) (*domain.ReleaseRun, error) {
	return scanRun(s.pool.QueryRow(ctx, selectRunByRequestSQL, requestID))
}

func (s *Store) UpdateRun(run *domain.ReleaseRun, expectedVersion int64) error {
	return s.UpdateRunAtomic(run, expectedVersion, nil, nil)
}

func (s *Store) UpdateRunAtomic(run *domain.ReleaseRun, expectedVersion int64, audits []domain.AuditEvent, outbox []domain.OutboxEvent) error {
	ctx := context.Background()
	updated := cloneRun(run)
	updated.StateVersion = expectedVersion + 1
	payload, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, updateRunSQL, updated.ID, updated.TemporalRunID, updated.Status, updated.StateVersion, payload, updated.UpdatedAt, expectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM release_runs WHERE id=$1)`, updated.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return store.ErrConflict
		}
		return store.ErrNotFound
	}
	if err := syncChildren(ctx, tx, updated); err != nil {
		return err
	}
	if err := insertEvents(ctx, tx, audits, appendProjectionEvent(outbox, updated)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	run.StateVersion = updated.StateVersion
	return nil
}

func (s *Store) AppendAudit(event domain.AuditEvent) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := insertAudit(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AuditEvents(correlationID string) ([]domain.AuditEvent, error) {
	rows, err := s.pool.Query(context.Background(), `
SELECT id,correlation_id,actor_type,actor_id,COALESCE(delegated_by,''),action,
       resource_type,resource_id,result,details,occurred_at
FROM audit_events WHERE correlation_id=$1 ORDER BY sequence`, correlationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []domain.AuditEvent
	for rows.Next() {
		var event domain.AuditEvent
		var details []byte
		if err := rows.Scan(&event.ID, &event.CorrelationID, &event.ActorType, &event.ActorID, &event.DelegatedBy,
			&event.Action, &event.ResourceType, &event.ResourceID, &event.Result, &details, &event.Timestamp); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(details, &event.Details); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func syncChildren(ctx context.Context, tx pgx.Tx, run *domain.ReleaseRun) error {
	for _, step := range run.Steps {
		payload, err := json.Marshal(step)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, upsertStepSQL, run.ID, step.Service, run.Environment, step.Phase, step.Status, run.StateVersion, payload, run.UpdatedAt); err != nil {
			return err
		}
		if step.Policy != nil {
			snapshot, _ := json.Marshal(step.Policy)
			id := fmt.Sprintf("policy/%s/%s/%s", run.ID, step.Service, step.Policy.InputHash)
			if _, err := tx.Exec(ctx, `
INSERT INTO policy_decisions (id,run_id,service,decision,policy_version,input_hash,reason_code,snapshot,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (id) DO NOTHING`,
				id, run.ID, step.Service, step.Policy.Decision, step.Policy.PolicyVersion, step.Policy.InputHash, step.Policy.ReasonCode, snapshot, step.Policy.CreatedAt); err != nil {
				return err
			}
		}
		if step.Approval != nil {
			roles, _ := json.Marshal(step.Approval.RequiredRoles)
			if _, err := tx.Exec(ctx, `
INSERT INTO approvals (id,run_id,service,status,plan_hash,required_roles,requested_at,expires_at,decided_by,decided_at,state_version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11)
ON CONFLICT (id) DO UPDATE SET status=EXCLUDED.status,decided_by=EXCLUDED.decided_by,
decided_at=EXCLUDED.decided_at,state_version=EXCLUDED.state_version`,
				step.Approval.ID, run.ID, step.Service, step.Approval.Status, step.Approval.PlanHash, roles,
				step.Approval.RequestedAt, step.Approval.ExpiresAt, step.Approval.DecidedBy, step.Approval.DecidedAt, run.StateVersion); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertEvents(ctx context.Context, tx pgx.Tx, audits []domain.AuditEvent, outbox []domain.OutboxEvent) error {
	for _, event := range audits {
		if err := insertAudit(ctx, tx, event); err != nil {
			return err
		}
	}
	for _, event := range outbox {
		payload, err := json.Marshal(event.Payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, insertOutboxSQL, event.ID, event.AggregateType, event.AggregateID, event.EventType, payload, event.Attempts, event.AvailableAt, event.PublishedAt, event.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func insertAudit(ctx context.Context, tx pgx.Tx, event domain.AuditEvent) error {
	details, err := json.Marshal(event.Details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, insertAuditSQL, event.ID, event.CorrelationID, event.ActorType, event.ActorID, event.DelegatedBy,
		event.Action, event.ResourceType, event.ResourceID, event.Result, details, event.Timestamp)
	return err
}

func scanRun(row pgx.Row) (*domain.ReleaseRun, error) {
	var payload []byte
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	var run domain.ReleaseRun
	if err := json.Unmarshal(payload, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func cloneRun(run *domain.ReleaseRun) *domain.ReleaseRun {
	data, err := json.Marshal(run)
	if err != nil {
		panic(err)
	}
	var result domain.ReleaseRun
	if err := json.Unmarshal(data, &result); err != nil {
		panic(err)
	}
	return &result
}

func appendProjectionEvent(events []domain.OutboxEvent, run *domain.ReleaseRun) []domain.OutboxEvent {
	payload, _ := json.Marshal(run)
	event := domain.OutboxEvent{
		ID: fmt.Sprintf("projection/%s/%d", run.ID, run.StateVersion), AggregateType: "release_run", AggregateID: run.ID,
		EventType: "release.run.project", Payload: map[string]any{"run": json.RawMessage(payload)}, CreatedAt: run.UpdatedAt, AvailableAt: run.UpdatedAt,
	}
	return append(append([]domain.OutboxEvent(nil), events...), event)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) GetRunnerAction(ctx context.Context, tenantID, runnerGroup, nonce string) (store.RunnerActionRecord, error) {
	return scanRunnerAction(s.pool.QueryRow(ctx, `
SELECT `+runnerActionColumns+`
FROM runner_journal WHERE tenant_id=$1 AND runner_group=$2 AND nonce=$3`, tenantID, runnerGroup, nonce))
}

func (s *Store) ReserveRunnerAction(ctx context.Context, record store.RunnerActionRecord, audit domain.AuditEvent) (store.RunnerActionRecord, bool, error) {
	record.Status, record.StateVersion = store.RunnerActionReserved, 1
	result, err := json.Marshal(record.Result)
	if err != nil {
		return store.RunnerActionRecord{}, false, err
	}
	target, err := json.Marshal(record.Target)
	if err != nil {
		return store.RunnerActionRecord{}, false, err
	}
	action, err := json.Marshal(record.Action)
	if err != nil {
		return store.RunnerActionRecord{}, false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.RunnerActionRecord{}, false, err
	}
	defer tx.Rollback(ctx)
	row := tx.QueryRow(ctx, `
INSERT INTO runner_journal (grant_id,run_id,step_id,tenant_id,runner_group,nonce,idempotency_key,request_hash,target,action,status,result,state_version,created_at,updated_at,runner_id,plan_revision,plan_hash)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
ON CONFLICT DO NOTHING
RETURNING `+runnerActionColumns,
		record.GrantID, record.RunID, record.StepID, record.TenantID, record.RunnerGroup, record.Nonce,
		record.IdempotencyKey, record.RequestHash, target, action, record.Status, result, record.StateVersion, record.CreatedAt, record.UpdatedAt, record.RunnerID, record.PlanRevision, record.PlanHash)
	createdRecord, scanErr := scanRunnerAction(row)
	if scanErr == nil {
		if err := insertAudit(ctx, tx, audit); err != nil {
			return store.RunnerActionRecord{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return store.RunnerActionRecord{}, false, err
		}
		return createdRecord, true, nil
	}
	if !errors.Is(scanErr, store.ErrNotFound) {
		return store.RunnerActionRecord{}, false, scanErr
	}
	existing, err := scanRunnerAction(tx.QueryRow(ctx, `
SELECT `+runnerActionColumns+`
FROM runner_journal
WHERE tenant_id=$1 AND runner_group=$2 AND (nonce=$3 OR idempotency_key=$4)
ORDER BY CASE WHEN nonce=$3 THEN 0 ELSE 1 END LIMIT 1`, record.TenantID, record.RunnerGroup, record.Nonce, record.IdempotencyKey))
	if err != nil {
		return store.RunnerActionRecord{}, false, err
	}
	return existing, false, nil
}

func (s *Store) CompleteRunnerAction(ctx context.Context, record store.RunnerActionRecord, expected int64, audit domain.AuditEvent) error {
	result, err := json.Marshal(record.Result)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if record.Status == store.RunnerActionDispatched && record.PlanRevision > 0 {
		// Serialize dispatch admission with plan activation. A changed plan can
		// fence a reservation; it cannot retract a provider call already admitted.
		var revision int64
		var hash string
		if err := tx.QueryRow(ctx, `SELECT revision,plan_hash FROM plan_revisions WHERE tenant_id=$1 AND runner_group=$2 AND run_id=$3 FOR SHARE`, record.TenantID, record.RunnerGroup, record.RunID).Scan(&revision, &hash); err != nil {
			return err
		}
		if revision != record.PlanRevision || hash != record.PlanHash {
			return store.ErrConflict
		}
	}
	tag, err := tx.Exec(ctx, `
UPDATE runner_journal SET status=$1,result=$2,state_version=$3,updated_at=$4,reconcile_owner='',reconcile_until=NULL
WHERE tenant_id=$5 AND runner_group=$6 AND nonce=$7 AND state_version=$8
 AND request_hash=$9 AND runner_id=$10
 AND status IN ('RESERVED','DISPATCHED','UNKNOWN')
 AND ($1 <> 'DISPATCHED' OR (status='RESERVED' AND reconcile_owner=''))`,
		record.Status, result, expected+1, record.UpdatedAt, record.TenantID, record.RunnerGroup, record.Nonce, expected, record.RequestHash, record.RunnerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrConflict
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PendingRunnerActions(ctx context.Context, tenantID, runnerGroup string, limit int) ([]store.RunnerActionRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
SELECT `+runnerActionColumns+`
FROM runner_journal WHERE tenant_id=$1 AND runner_group=$2 AND status IN ('RESERVED','DISPATCHED','UNKNOWN') ORDER BY updated_at LIMIT $3`, tenantID, runnerGroup, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []store.RunnerActionRecord{}
	for rows.Next() {
		record, err := scanRunnerAction(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

type runnerActionRow interface{ Scan(...any) error }

func scanRunnerAction(row runnerActionRow) (store.RunnerActionRecord, error) {
	var record store.RunnerActionRecord
	var until *time.Time
	var target, action, result []byte
	if err := row.Scan(&record.GrantID, &record.RunID, &record.StepID, &record.TenantID, &record.RunnerGroup,
		&record.Nonce, &record.IdempotencyKey, &record.RequestHash, &target, &action, &record.Status, &result, &record.StateVersion,
		&record.CreatedAt, &record.UpdatedAt, &record.RunnerID, &record.ReconcileOwner, &until, &record.PlanRevision, &record.PlanHash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.RunnerActionRecord{}, store.ErrNotFound
		}
		return store.RunnerActionRecord{}, err
	}
	if until != nil {
		record.ReconcileUntil = *until
	}
	if len(target) > 0 {
		if err := json.Unmarshal(target, &record.Target); err != nil {
			return store.RunnerActionRecord{}, err
		}
	}
	if len(action) > 0 {
		if err := json.Unmarshal(action, &record.Action); err != nil {
			return store.RunnerActionRecord{}, err
		}
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &record.Result); err != nil {
			return store.RunnerActionRecord{}, err
		}
	}
	return record, nil
}

var _ store.DurableStore = (*Store)(nil)
