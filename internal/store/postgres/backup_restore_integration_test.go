//go:build integration

package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/protocol"

	"github.com/jackc/pgx/v5/pgxpool"
)

const backupTenant = "backup-fixture-tenant"
const backupGroup = "backup-fixture-group"
const backupRun = "backup-fixture-run"

// These entry points are called only by the existing backup/restore drill.
// The shell creates new databases first and owns all cleanup. This code never
// drops a database, truncates tables or connects to the normal Compose database.
func backupDatabaseURL(t *testing.T, name, prefix string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skip("requires explicitly created disposable backup databases")
	}
	config, err := pgxpool.ParseConfig(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(config.ConnConfig.Database, prefix) {
		t.Fatal("refusing a database outside the disposable backup prefix")
	}
	return value
}

func TestPrepareRunnerBackupRetentionFixture(t *testing.T) {
	url := backupDatabaseURL(t, "THEMISY_BACKUP_SOURCE_DATABASE_URL", "themisy_backup_source_")
	ctx := context.Background()
	if err := MigrateRunner(ctx, url); err != nil {
		t.Fatal(err)
	}
	journal, err := NewRunnerJournal(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	var existing int
	if err := journal.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM runner_journal)+(SELECT count(*) FROM plan_manifests)+(SELECT count(*) FROM audit_events)`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Fatal("fixture source is not empty; refusing to overwrite or reuse it")
	}
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("a", 64)
	latest := "sha256:" + strings.Repeat("b", 64)
	signer, _, err := grant.NewDevelopmentSigner("synthetic-backup-key")
	if err != nil {
		t.Fatal(err)
	}
	for revision := int64(1); revision <= 2; revision++ {
		planHash := digest
		if revision == 2 {
			planHash = latest
		}
		manifest := protocol.PlanManifest{ProtocolVersion: protocol.PlanManifestVersion, Issuer: "example://backup-fixture", TenantID: backupTenant, RunnerGroup: backupGroup, RunID: backupRun, StepID: "identity", Revision: revision, PlanHash: planHash, ContractHash: digest, ProfileHash: digest, PolicyHash: digest, EvidenceHash: digest, Target: protocol.Target{Service: "identity", Environment: "staging"}, Action: protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: digest}, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
		signed, err := grant.SignPlanManifest(ctx, signer, manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.PinManifest(ctx, signed); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []store.RunnerActionStatus{store.RunnerActionSucceeded, store.RunnerActionUnknown} {
		id := "backup-" + strings.ToLower(string(status))
		audit := func(action string) domain.AuditEvent {
			outcome := string(status)
			if strings.HasSuffix(action, ".reserved") {
				outcome = string(store.RunnerActionReserved)
			}
			if strings.HasSuffix(action, ".dispatched") {
				outcome = string(store.RunnerActionDispatched)
			}
			return domain.AuditEvent{ID: id + "/" + action, CorrelationID: backupRun, ActorType: "runner", ActorID: "synthetic-backup-runner", Action: action, ResourceType: "runner_action", ResourceID: id, Result: outcome, Details: map[string]any{"fixture": true, "status": string(status)}, Timestamp: now}
		}
		row, fresh, err := journal.ReserveRunnerAction(ctx, store.RunnerActionRecord{GrantID: id, RunID: backupRun, StepID: "identity", TenantID: backupTenant, RunnerGroup: backupGroup, Nonce: id + "-nonce", IdempotencyKey: id + "-key", RequestHash: digest, PlanRevision: 2, PlanHash: latest, Target: protocol.Target{Service: "identity", Environment: "staging"}, Action: protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: digest}, RunnerID: "synthetic-backup-runner", CreatedAt: now, UpdatedAt: now}, audit("backup.fixture.reserved"))
		if err != nil || !fresh {
			t.Fatalf("reserve fixture fresh=%v err=%v", fresh, err)
		}
		row.Status = store.RunnerActionDispatched
		if err := journal.CompleteRunnerAction(ctx, row, row.StateVersion, audit("backup.fixture.dispatched")); err != nil {
			t.Fatal(err)
		}
		row, err = journal.GetRunnerAction(ctx, backupTenant, backupGroup, row.Nonce)
		if err != nil {
			t.Fatal(err)
		}
		resultStatus := protocol.ResultSucceeded
		if status == store.RunnerActionUnknown {
			resultStatus = protocol.ResultUnknown
		}
		row.Status = status
		row.Result = protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: id, RunID: backupRun, StepID: "identity", Status: resultStatus, ReasonCode: "SYNTHETIC_BACKUP_FIXTURE", ExternalExecutionID: id + "-external", CompletedAt: now.Add(time.Second)}
		row.UpdatedAt = now.Add(time.Second)
		if err := journal.CompleteRunnerAction(ctx, row, row.StateVersion, audit("backup.fixture.completed")); err != nil {
			t.Fatal(err)
		}
		if status == store.RunnerActionSucceeded {
			saved, err := journal.GetRunnerAction(ctx, backupTenant, backupGroup, row.Nonce)
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.MarkRunnerReported(ctx, saved); err != nil {
				t.Fatal(err)
			}
		}
	}
}

const backupSnapshotSQL = `SELECT jsonb_build_object(
 'journal',(SELECT coalesce(jsonb_agg(to_jsonb(j) ORDER BY tenant_id,runner_group,nonce),'[]'::jsonb) FROM themisy_runner.runner_journal j),
 'manifests',(SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY tenant_id,runner_group,run_id,step_id,capability,revision),'[]'::jsonb) FROM themisy_runner.plan_manifests m),
 'revisions',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY tenant_id,runner_group,run_id),'[]'::jsonb) FROM themisy_runner.plan_revisions r),
 'audit',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY sequence),'[]'::jsonb) FROM themisy_runner.audit_events a),
 'schema_versions',(SELECT coalesce(jsonb_agg(to_jsonb(v) ORDER BY version),'[]'::jsonb) FROM themisy_runner.schema_versions v)
)`

func TestVerifyRunnerBackupRetention(t *testing.T) {
	sourceURL := backupDatabaseURL(t, "THEMISY_BACKUP_SOURCE_DATABASE_URL", "themisy_backup_source_")
	restoreURL := backupDatabaseURL(t, "THEMISY_BACKUP_RESTORE_DATABASE_URL", "themisy_backup_restore_")
	ctx := context.Background()
	source, err := NewRunnerJournal(ctx, sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	// Connect without relying on restored schema-version rows so a schema-only
	// restore reaches the data comparison and is explicitly rejected as mismatch.
	restored, err := New(ctx, restoreURL)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var succeeded, unknown, manifests, revisions, audits int
	if err := source.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM runner_journal WHERE status='SUCCEEDED'),
 (SELECT count(*) FROM runner_journal WHERE status='UNKNOWN'),
 (SELECT count(*) FROM plan_manifests),
 (SELECT count(*) FROM plan_revisions WHERE revision=2),
 (SELECT count(*) FROM audit_events)`).Scan(&succeeded, &unknown, &manifests, &revisions, &audits); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || unknown != 1 || manifests != 2 || revisions != 1 || audits != 8 {
		t.Fatalf("SOURCE_FIXTURE_INVALID: succeeded=%d unknown=%d manifests=%d revisions=%d audits=%d", succeeded, unknown, manifests, revisions, audits)
	}
	var before, after []byte
	if err := source.pool.QueryRow(ctx, backupSnapshotSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := restored.pool.QueryRow(ctx, backupSnapshotSQL).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("BACKUP_RETENTION_MISMATCH: restored Journal/Manifest/revision/Audit/schema-version payload differs")
	}
	t.Logf("retained source snapshot sha256:%x (2 Journal, 2 Manifest, revision 2, 8 Audit)", sha256.Sum256(before))
	restoredJournal, err := NewRunnerJournal(ctx, restoreURL)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredJournal.Close()
	for _, status := range []store.RunnerActionStatus{store.RunnerActionSucceeded, store.RunnerActionUnknown} {
		nonce := "backup-" + strings.ToLower(string(status)) + "-nonce"
		expected, err := source.GetRunnerAction(ctx, backupTenant, backupGroup, nonce)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := restoredJournal.GetRunnerAction(ctx, backupTenant, backupGroup, nonce)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(expected, actual) {
			t.Fatal("restored typed Journal result differs")
		}
		for _, sameNonce := range []bool{true, false} {
			reused := actual
			if sameNonce {
				reused.IdempotencyKey += "/different"
			} else {
				reused.Nonce += "/different"
			}
			duplicate, fresh, err := restoredJournal.ReserveRunnerAction(ctx, reused, domain.AuditEvent{ID: "must-not-be-inserted", Timestamp: time.Now().UTC()})
			if err != nil || fresh || duplicate.GrantID != actual.GrantID || duplicate.Status != status {
				t.Fatalf("restored reservation reused nonce=%t fresh=%t err=%v", sameNonce, fresh, err)
			}
		}
	}
	expectedManifest, err := source.GetManifest(ctx, backupTenant, backupGroup, backupRun, "identity", protocol.CapabilityDeploy)
	if err != nil {
		t.Fatal(err)
	}
	actualManifest, err := restoredJournal.GetManifest(ctx, backupTenant, backupGroup, backupRun, "identity", protocol.CapabilityDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if expectedManifest.Revision != 2 || !reflect.DeepEqual(expectedManifest, actualManifest) {
		t.Fatal("restored current Manifest revision differs")
	}
	// Duplicate attempts must not change status, counters or append new audits.
	if err := restored.pool.QueryRow(ctx, backupSnapshotSQL).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("duplicate reservation changed restored state")
	}
	fmt.Println("Backup retention and previously used nonce/idempotency rejection verified.")
}
