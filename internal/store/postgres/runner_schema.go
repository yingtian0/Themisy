package postgres

import (
	"context"
	"embed"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed runner_migrations/*.sql
var runnerMigrations embed.FS

// MigrateRunner uses a separate migration identity, schema and version table.
// Runtime Runner credentials need no CREATE privilege.
func MigrateRunner(ctx context.Context, databaseURL string) error {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(782019141); CREATE SCHEMA IF NOT EXISTS themisy_runner; SET LOCAL search_path TO themisy_runner; CREATE TABLE IF NOT EXISTS schema_versions(version text PRIMARY KEY)`); err != nil {
		return err
	}
	entries, err := runnerMigrations.ReadDir("runner_migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_versions WHERE version=$1)`, entry.Name()).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := runnerMigrations.ReadFile("runner_migrations/" + entry.Name())
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("runner migration %s: %w", entry.Name(), err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_versions VALUES($1)`, entry.Name()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func NewRunnerJournal(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "themisy_runner"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Never silently fall back to the Control Plane's public.runner_journal.
	var version string
	err = pool.QueryRow(ctx, `SELECT version FROM schema_versions ORDER BY version DESC LIMIT 1`).Scan(&version)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("Runner schema unavailable: %w", err)
	}
	if version < "004_plan_fence.sql" {
		pool.Close()
		return nil, fmt.Errorf("Runner schema upgrade required")
	}
	return &Store{pool: pool}, nil
}
