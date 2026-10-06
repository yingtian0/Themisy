//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/protocol"
)

func TestRunnerJournalSurvivesReconnectAndDeduplicatesNonce(t *testing.T) {
	requireIntegration(t)
	ctx := context.Background()
	databaseURL := integrationDatabaseURL()
	if err := postgresstore.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	persistent, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	suffix := now.Format("150405.000000000")
	record := store.RunnerActionRecord{GrantID: "runner-grant-" + suffix, RunID: "runner-run-" + suffix, StepID: "step-1", TenantID: "tenant-integration", RunnerGroup: "runner-integration", Nonce: "nonce-" + suffix, IdempotencyKey: "key-" + suffix, RequestHash: "sha256:integration", CreatedAt: now, UpdatedAt: now}
	audit := integrationAudit("runner-audit-"+suffix, record.GrantID, now)
	reserved, created, err := persistent.ReserveRunnerAction(ctx, record, audit)
	if err != nil || !created {
		t.Fatalf("reserve created=%v err=%v", created, err)
	}
	persistent.Close()

	reconnected, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	loaded, err := reconnected.GetRunnerAction(ctx, record.TenantID, record.RunnerGroup, record.Nonce)
	if err != nil || loaded.RequestHash != record.RequestHash {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	if _, created, err := reconnected.ReserveRunnerAction(ctx, record, integrationAudit("ignored-"+suffix, record.GrantID, now)); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	reserved.Status, reserved.UpdatedAt = store.RunnerActionSucceeded, now.Add(time.Second)
	if err := reconnected.CompleteRunnerAction(ctx, reserved, reserved.StateVersion, integrationAudit("runner-complete-"+suffix, record.GrantID, now)); err != nil {
		t.Fatal(err)
	}
	completed, err := reconnected.GetRunnerAction(ctx, record.TenantID, record.RunnerGroup, record.Nonce)
	if err != nil || completed.Status != store.RunnerActionSucceeded {
		t.Fatalf("completed=%#v err=%v", completed, err)
	}
}

func TestGrantDispatchSurvivesReconnectAndRecordsAckResult(t *testing.T) {
	requireIntegration(t)
	ctx := context.Background()
	databaseURL := integrationDatabaseURL()
	if err := postgresstore.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	persistent, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	suffix := now.Format("150405.000000000")
	run := &domain.ReleaseRun{ID: "grant-run-" + suffix, RequestID: "grant-request-" + suffix, WorkflowID: "grant-run-" + suffix, TenantID: "tenant-integration", StateVersion: 1, CreatedAt: now, UpdatedAt: now}
	if _, _, err := persistent.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	grant := protocol.ActionGrant{GrantID: "grant-" + suffix, RunID: run.ID, StepID: "payment-api", TenantID: run.TenantID, RunnerGroup: "runner-integration", Issuer: "https://control.example", IdempotencyKey: "grant-key-" + suffix, Nonce: "grant-nonce-" + suffix, ExpiresAt: now.Add(time.Minute)}
	outbox := domain.OutboxEvent{ID: "grant-outbox-" + suffix, AggregateType: "action_grant", AggregateID: grant.GrantID, EventType: "grant.dispatch.requested", CreatedAt: now, AvailableAt: now}
	created, fresh, err := persistent.CreateGrantDispatch(ctx, store.GrantDispatchRecord{Grant: grant, CreatedAt: now, UpdatedAt: now}, integrationAudit("grant-issue-"+suffix, run.ID, now), outbox)
	if err != nil || !fresh || created.Status != store.GrantDispatchPending {
		t.Fatalf("create=%#v fresh=%v err=%v", created, fresh, err)
	}
	claimed, err := persistent.ClaimGrantDispatch(ctx, grant.TenantID, grant.RunnerGroup, "runner-1", "delivery-token", now, now.Add(time.Minute))
	if err != nil || claimed.DeliveryToken != "delivery-token" {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	persistent.Close()

	reconnected, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	acked, err := reconnected.AcknowledgeGrantDispatch(ctx, grant.GrantID, "runner-1", claimed.DeliveryToken, now.Add(time.Second), now.Add(2*time.Minute))
	if err != nil || acked.Status != store.GrantDispatchAcked {
		t.Fatalf("ack=%#v err=%v", acked, err)
	}
	result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: grant.GrantID, RunID: run.ID, StepID: grant.StepID, Status: protocol.ResultSucceeded, CompletedAt: now.Add(2 * time.Second)}
	unknown := result
	unknown.Status = protocol.ResultUnknown
	uncertain, err := reconnected.CompleteGrantDispatch(ctx, grant.GrantID, "runner-1", claimed.DeliveryToken, unknown, unknown.CompletedAt, integrationAudit("grant-unknown-"+suffix, run.ID, now))
	if err != nil {
		t.Fatal(err)
	}
	if err = reconnected.ReconcileGrantDispatch(ctx, grant.GrantID, uncertain.StateVersion, result, integrationAudit("grant-recovery-"+suffix, run.ID, now.Add(3*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err = reconnected.ReconcileGrantDispatch(ctx, grant.GrantID, uncertain.StateVersion, result, integrationAudit("grant-recovery-replay-"+suffix, run.ID, now.Add(4*time.Second))); err != nil {
		t.Fatalf("idempotent recovery: %v", err)
	}
	completed, err := reconnected.GetGrantDispatch(ctx, grant.GrantID)
	if err != nil || completed.Status != store.GrantDispatchSucceeded {
		t.Fatalf("complete=%#v err=%v", completed, err)
	}
	if err := reconnected.ReconcileGrantDispatch(ctx, grant.GrantID, completed.StateVersion, unknown, integrationAudit("grant-recovery-conflict-"+suffix, run.ID, now)); err != store.ErrConflict {
		t.Fatalf("terminal conflict accepted: %v", err)
	}
}

func TestRunnerFleetHeartbeatAndDrainSurviveControlPlaneRestart(t *testing.T) {
	requireIntegration(t)
	ctx := context.Background()
	databaseURL := integrationDatabaseURL()
	if err := postgresstore.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	persistent, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	suffix := now.Format("150405.000000000")
	runnerID := "runner-fleet-" + suffix
	correlationID := "runner/tenant-integration/" + runnerID
	heartbeat := domain.RunnerInfo{ID: runnerID, TenantID: "tenant-integration", Group: "runner-integration", ReportedCapacity: 3, InFlight: 1, LastSeen: now}
	connected, err := persistent.HeartbeatRunner(ctx, heartbeat, integrationAudit("runner-connect-"+suffix, correlationID, now))
	if err != nil || connected.Status != domain.RunnerReady || connected.Capacity != 2 {
		t.Fatalf("connected=%#v err=%v", connected, err)
	}
	drained, err := persistent.SetRunnerStatus(ctx, heartbeat.TenantID, runnerID, domain.RunnerDraining, "operator", now.Add(time.Second), integrationAudit("runner-drain-"+suffix, correlationID, now.Add(time.Second)))
	if err != nil || drained.Capacity != 0 {
		t.Fatalf("drained=%#v err=%v", drained, err)
	}
	persistent.Close()

	reconnected, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	loaded, err := reconnected.GetRunner(ctx, heartbeat.TenantID, runnerID)
	if err != nil || loaded.Status != domain.RunnerDraining || loaded.Capacity != 0 {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	for i, status := range []domain.RunnerStatus{domain.RunnerFrozen, domain.RunnerReady, domain.RunnerDraining} {
		changedAt := now.Add(time.Duration(i+2) * time.Second).Truncate(time.Microsecond)
		changed, err := reconnected.SetRunnerStatus(ctx, heartbeat.TenantID, runnerID, status, "operator", changedAt, integrationAudit("runner-control-"+string(status)+suffix, correlationID, changedAt))
		if err != nil {
			t.Fatal(err)
		}
		// Reconnect to check persisted controls, not process-local state.
		restarted, err := postgresstore.New(ctx, databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		heartbeat.LastSeen = changedAt.Add(time.Second)
		got, err := restarted.HeartbeatRunner(ctx, heartbeat, domain.AuditEvent{})
		restarted.Close()
		if err != nil || got.Status != status || got.ControlledBy != "operator" || got.ControlledAt == nil || !got.ControlledAt.Equal(changedAt) || got.StateVersion != changed.StateVersion+1 {
			t.Fatalf("restart status=%s got=%#v err=%v", status, got, err)
		}
		wantCapacity := 0
		if status == domain.RunnerReady {
			wantCapacity = 2
		}
		if got.Capacity != wantCapacity {
			t.Fatalf("capacity=%d want=%d", got.Capacity, wantCapacity)
		}
		if status == domain.RunnerFrozen {
			if got.FrozenBy != "operator" || got.FrozenAt == nil || !got.FrozenAt.Equal(changedAt) {
				t.Fatalf("missing freeze metadata: %#v", got)
			}
		} else if got.FrozenBy != "" || got.FrozenAt != nil {
			t.Fatalf("stale freeze metadata: %#v", got)
		}
	}
}
