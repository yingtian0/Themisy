//go:build integration

package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
	"sync"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/protocol"
	"time"
)

func recoveryURL() string {
	if u := os.Getenv("THEMISY_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://themisy:themisy@localhost:5432/themisy?sslmode=disable"
}

func TestSharedJournalProcessesAndCrashRecovery(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	ctx := context.Background()
	url := recoveryURL()
	if err := postgresstore.MigrateRunner(ctx, url); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS recovery_test_writes(test_id text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, crash := range []string{"none", "reserved", "before-write", "after-write", "after-response"} {
		t.Run(crash, func(t *testing.T) {
			id := fmt.Sprintf("recovery-%d", time.Now().UnixNano())
			command := func(mode, replica string) *exec.Cmd {
				c := exec.Command(os.Args[0], "-test.run=^TestRecoveryProcessHelper$", "-test.count=1")
				c.Env = append(os.Environ(), "THEMISY_RECOVERY_HELPER="+mode, "THEMISY_RECOVERY_ID="+id, "THEMISY_RECOVERY_REPLICA="+replica)
				return c
			}
			if crash != "none" {
				out, err := command(crash, "crashed").CombinedOutput()
				if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 42 {
					t.Fatalf("crash helper: %v %s", err, out)
				}
			}
			one, two := command("none", "one"), command("none", "two")
			if err := one.Start(); err != nil {
				t.Fatal(err)
			}
			if err := two.Start(); err != nil {
				t.Fatal(err)
			}
			if err := one.Wait(); err != nil {
				t.Fatal(err)
			}
			if err := two.Wait(); err != nil {
				t.Fatal(err)
			}
			// A new nonce does not evade the second, independent uniqueness key.
			duplicate := command("none", "different-nonce")
			duplicate.Env = append(duplicate.Env, "THEMISY_RECOVERY_NONCE=another-nonce")
			if output, err := duplicate.CombinedOutput(); err != nil {
				t.Fatalf("idempotency replay: %v %s", err, output)
			}
			var writes int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM recovery_test_writes WHERE test_id=$1`, id).Scan(&writes); err != nil {
				t.Fatal(err)
			}
			want := 1
			if crash == "reserved" || crash == "before-write" {
				want = 0
			}
			if writes != want {
				t.Fatalf("provider writes=%d want=%d", writes, want)
			}
			journal, err := postgresstore.NewRunnerJournal(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			claimed, err := journal.ClaimRunnerActions(ctx, "tenant-1", id, "reconciler-one", time.Now(), time.Now().Add(time.Minute), 10)
			if err != nil {
				t.Fatal(err)
			}
			other, err := journal.ClaimRunnerActions(ctx, "tenant-1", id, "reconciler-two", time.Now(), time.Now().Add(time.Minute), 10)
			if err != nil || len(other) != 0 {
				t.Fatalf("duplicate claim %#v %v", other, err)
			}
			if crash != "none" && len(claimed) != 1 {
				t.Fatalf("pending records=%d", len(claimed))
			}
			if len(claimed) > 0 {
				r := claimed[0]
				r.Status = store.RunnerActionUnknown
				r.UpdatedAt = time.Now()
				stale := r.StateVersion - 1
				event := domain.AuditEvent{ID: id + "-resolve", CorrelationID: id, Timestamp: time.Now()}
				if err := journal.CompleteRunnerAction(ctx, r, stale, event); err != store.ErrConflict {
					t.Fatalf("stale version accepted: %v", err)
				}
				if err := journal.CompleteRunnerAction(ctx, r, r.StateVersion, event); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestConcurrentDifferentNoncesShareOneIdempotencyReservation(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	ctx := context.Background()
	url := recoveryURL()
	if err := postgresstore.MigrateRunner(ctx, url); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS recovery_test_writes(test_id text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS recovery_test_ready(test_id text NOT NULL, replica text NOT NULL, PRIMARY KEY(test_id, replica))`); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("idem-race-%d", time.Now().UnixNano())
	command := func(nonce string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryProcessHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "THEMISY_RECOVERY_HELPER=none", "THEMISY_RECOVERY_ID="+id, "THEMISY_RECOVERY_REPLICA="+nonce, "THEMISY_RECOVERY_NONCE="+nonce)
		cmd.Env = append(cmd.Env, "THEMISY_RECOVERY_IDEMPOTENCY_KEY="+id+"-shared-key")
		return cmd
	}
	one, two := command("nonce-one"), command("nonce-two")
	var firstOutput, secondOutput, firstError, secondError bytes.Buffer
	one.Stdout, one.Stderr = &firstOutput, &firstError
	two.Stdout, two.Stderr = &secondOutput, &secondError
	if err := one.Start(); err != nil {
		t.Fatal(err)
	}
	if err := two.Start(); err != nil {
		_ = one.Process.Kill()
		_ = one.Wait()
		t.Fatal(err)
	}
	if err := one.Wait(); err != nil {
		t.Fatalf("first Runner process: %v stdout=%s stderr=%s", err, firstOutput.String(), firstError.String())
	}
	if err := two.Wait(); err != nil {
		t.Fatalf("second Runner process: %v stdout=%s stderr=%s", err, secondOutput.String(), secondError.String())
	}
	var firstResult, secondResult protocol.Result
	if err := json.NewDecoder(&firstOutput).Decode(&firstResult); err != nil {
		t.Fatalf("decode first Runner result: %v (%s)", err, firstOutput.String())
	}
	if err := json.NewDecoder(&secondOutput).Decode(&secondResult); err != nil {
		t.Fatalf("decode second Runner result: %v (%s)", err, secondOutput.String())
	}
	results := []protocol.Result{firstResult, secondResult}
	succeeded, replayResponses := 0, 0
	for _, result := range results {
		switch {
		case result.Status == protocol.ResultSucceeded:
			succeeded++
		case (result.Status == protocol.ResultRejected || result.Status == protocol.ResultReconciliation) && result.ReasonCode == string(ReasonReplay):
			replayResponses++
		}
	}
	if succeeded != 1 || replayResponses != 1 {
		t.Fatalf("Runner results=%+v, want one success and one replay response", results)
	}
	var writes, reservations int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM recovery_test_writes WHERE test_id=$1`, id).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM themisy_runner.runner_journal WHERE runner_group=$1 AND idempotency_key=$2`, id, id+"-shared-key").Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || reservations != 1 {
		rows, queryErr := conn.Query(ctx, `SELECT tenant_id, runner_group, grant_id, nonce, idempotency_key FROM themisy_runner.runner_journal WHERE grant_id=$1 OR runner_group=$2 OR idempotency_key=$3`, id, id, id+"-shared-key")
		if queryErr != nil {
			t.Fatalf("writes=%d idempotency reservations=%d, queryErr=%v", writes, reservations, queryErr)
		}
		defer rows.Close()
		var observed []string
		for rows.Next() {
			var tenant, group, grantID, nonce, key string
			if err := rows.Scan(&tenant, &group, &grantID, &nonce, &key); err != nil {
				t.Fatal(err)
			}
			observed = append(observed, fmt.Sprintf("tenant=%s group=%s grant=%s nonce=%s key=%s", tenant, group, grantID, nonce, key))
		}
		t.Fatalf("writes=%d idempotency reservations=%d, journal rows=%v want one each", writes, reservations, observed)
	}
}

func TestRunnerReservationRollsBackWhenAuditInsertFails(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	ctx := context.Background()
	url := recoveryURL()
	if err := postgresstore.MigrateRunner(ctx, url); err != nil {
		t.Fatal(err)
	}
	journal, err := postgresstore.NewRunnerJournal(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	id := fmt.Sprintf("audit-atomic-%d", time.Now().UnixNano())
	now := time.Now().UTC()
	event := domain.AuditEvent{ID: id + "-reserve", CorrelationID: id, ActorType: "runner", ActorID: "runner-1", Action: "runner.action.reserve", ResourceType: "runner_action", ResourceID: id, Result: "RESERVED", Timestamp: now}
	if err := journal.AppendAudit(event); err != nil {
		t.Fatal(err)
	}
	record := store.RunnerActionRecord{GrantID: id, RunID: id, StepID: "payment-api", TenantID: "tenant-atomic", RunnerGroup: "group-atomic", Nonce: id + "-nonce", IdempotencyKey: id + "-key", RequestHash: "request-hash", RunnerID: "runner-1", CreatedAt: now, UpdatedAt: now}
	if _, created, err := journal.ReserveRunnerAction(ctx, record, event); err == nil || created {
		t.Fatalf("duplicate audit ID did not abort reservation: created=%t err=%v", created, err)
	}
	if _, err := journal.GetRunnerAction(ctx, record.TenantID, record.RunnerGroup, record.Nonce); err != store.ErrNotFound {
		t.Fatalf("reservation survived audit failure: err=%v", err)
	}
	events, err := journal.AuditEvents(id)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit rows=%d err=%v, want only preexisting duplicate", len(events), err)
	}
}

func TestConcurrentReconcileClaimsFenceExpiredWorker(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	ctx := context.Background()
	url := recoveryURL()
	if err := postgresstore.MigrateRunner(ctx, url); err != nil {
		t.Fatal(err)
	}
	journal, err := postgresstore.NewRunnerJournal(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	id := fmt.Sprintf("claim-fence-%d", time.Now().UnixNano())
	now := time.Now().UTC()
	record := store.RunnerActionRecord{GrantID: id, RunID: id, StepID: "payment-api", TenantID: "tenant-claim", RunnerGroup: id, Nonce: id + "-nonce", IdempotencyKey: id + "-key", RequestHash: "request-hash", RunnerID: "writer", CreatedAt: now, UpdatedAt: now}
	record, created, err := journal.ReserveRunnerAction(ctx, record, domain.AuditEvent{ID: id + "-reserve", CorrelationID: id, ActorType: "runner", ActorID: "writer", Action: "runner.action.reserve", ResourceType: "runner_action", ResourceID: id, Result: "RESERVED", Timestamp: now})
	if err != nil || !created {
		t.Fatalf("reserve created=%t err=%v", created, err)
	}

	type claimResult struct {
		records []store.RunnerActionRecord
		err     error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	var workers sync.WaitGroup
	for _, owner := range []string{"reconciler-a", "reconciler-b"} {
		workers.Add(1)
		go func(owner string) {
			defer workers.Done()
			<-start
			records, err := journal.ClaimRunnerActions(ctx, record.TenantID, record.RunnerGroup, owner, now, now.Add(time.Minute), 1)
			results <- claimResult{records: records, err: err}
		}(owner)
	}
	close(start)
	workers.Wait()
	close(results)

	var first store.RunnerActionRecord
	claimed := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.records) == 1 {
			first = result.records[0]
			claimed++
		} else if len(result.records) != 0 {
			t.Fatalf("worker claimed %d records", len(result.records))
		}
	}
	if claimed != 1 {
		t.Fatalf("simultaneous workers claimed=%d want=1", claimed)
	}

	// Once the lease expires, a new worker may claim for read-only recovery.
	// The former owner must remain fenced by state_version.
	later := now.Add(2 * time.Minute)
	second, err := journal.ClaimRunnerActions(ctx, record.TenantID, record.RunnerGroup, "reconciler-c", later, later.Add(time.Minute), 1)
	if err != nil || len(second) != 1 || second[0].StateVersion != first.StateVersion+1 {
		t.Fatalf("replacement claim=%#v err=%v", second, err)
	}
	stale := first
	stale.Status = store.RunnerActionUnknown
	stale.UpdatedAt = later
	staleAudit := domain.AuditEvent{ID: id + "-stale", CorrelationID: id, ActorType: "runner", ActorID: "stale-owner", Action: "runner.reconcile.unresolved", ResourceType: "runner_action", ResourceID: id, Result: "UNKNOWN", Timestamp: later}
	if err := journal.CompleteRunnerAction(ctx, stale, first.StateVersion, staleAudit); err != store.ErrConflict {
		t.Fatalf("expired worker update was not fenced: %v", err)
	}
}

func TestRecoveryProcessHelper(t *testing.T) {
	mode := os.Getenv("THEMISY_RECOVERY_HELPER")
	if mode == "" {
		return
	}
	id := os.Getenv("THEMISY_RECOVERY_ID")
	ctx := context.Background()
	journal, err := postgresstore.NewRunnerJournal(ctx, recoveryURL())
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	f := newRunnerFixture(t)
	f.grant.GrantID = id
	if nonce := os.Getenv("THEMISY_RECOVERY_NONCE"); nonce != "" {
		f.grant.Nonce = nonce
	}
	if idempotencyKey := os.Getenv("THEMISY_RECOVERY_IDEMPOTENCY_KEY"); idempotencyKey != "" {
		f.grant.IdempotencyKey = idempotencyKey
	}
	f.grant.RunnerGroup = id
	f.runner.RunnerID = os.Getenv("THEMISY_RECOVERY_REPLICA")
	f.runner.RunnerGroup = id
	// The fixture uses genuine signed Grants; each process verifies its own
	// equivalent canonical payload, whose hash excludes the signature.
	f.runner.Grants = &testGrantVerifier{fixture: f}
	f.resignWithPolicyHash(t)
	f.runner.Journal = &crashJournal{RunnerJournal: journal, mode: mode}
	f.runner.Adapter = &processProvider{mode: mode, id: id}
	if encoded := os.Getenv("THEMISY_RECOVERY_MANIFEST"); encoded != "" {
		var p protocol.PlanManifest
		if err := json.Unmarshal([]byte(encoded), &p); err != nil {
			t.Fatal(err)
		}
		key, err := base64.RawURLEncoding.DecodeString(os.Getenv("THEMISY_RECOVERY_MANIFEST_KEY"))
		if err != nil {
			t.Fatal(err)
		}
		m := &ManifestContexts{Store: journal, Issuer: p.Issuer, TenantID: p.TenantID, RunnerGroup: p.RunnerGroup, Keys: grant.StaticKeys{p.Issuer + "\x00grant-key": key}, Now: func() time.Time { return f.now }}
		f.runner.Contexts = m
		f.runner.Approvals = m
		// Do not pin in the child: both independent processes must read the
		// previously provisioned shared record after the provisioner closed.
	}
	if os.Getenv("THEMISY_RECOVERY_IDEMPOTENCY_KEY") != "" {
		replica := os.Getenv("THEMISY_RECOVERY_REPLICA")
		barrier, err := pgx.Connect(ctx, recoveryURL())
		if err != nil {
			t.Fatal(err)
		}
		defer barrier.Close(ctx)
		if _, err := barrier.Exec(ctx, `INSERT INTO recovery_test_ready(test_id, replica) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, replica); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			var ready int
			if err := barrier.QueryRow(ctx, `SELECT count(*) FROM recovery_test_ready WHERE test_id=$1`, id).Scan(&ready); err != nil {
				t.Fatal(err)
			}
			if ready == 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("concurrent recovery peers ready=%d want=2", ready)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	result, err := f.runner.Execute(ctx, f.grant)
	if os.Getenv("THEMISY_RECOVERY_IDEMPOTENCY_KEY") == "" {
		// Legacy crash/recovery scenarios intentionally inspect durable effects
		// from the parent and historically ignored child replay responses.
		return
	}
	if err != nil {
		var rejection *Rejection
		if errors.As(err, &rejection) && rejection.Reason == ReasonReplay && (result.Status == protocol.ResultRejected || result.Status == protocol.ResultReconciliation) && result.ReasonCode == string(ReasonReplay) {
			_ = json.NewEncoder(os.Stdout).Encode(result)
			return
		}
		t.Fatalf("Runner.Execute: result=%+v err=%v", result, err)
	}
	if result.Status != protocol.ResultSucceeded {
		t.Fatalf("Runner.Execute status=%s want=%s", result.Status, protocol.ResultSucceeded)
	}
	if os.Getenv("THEMISY_RECOVERY_IDEMPOTENCY_KEY") != "" {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSharedManifestSurvivesRestartAndReplicas(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	ctx := context.Background()
	setup, err := pgx.Connect(ctx, recoveryURL())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = setup.Exec(ctx, `CREATE TABLE IF NOT EXISTS recovery_test_writes(test_id text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	setup.Close(ctx)
	if err := postgresstore.MigrateRunner(ctx, recoveryURL()); err != nil {
		t.Fatal(err)
	}
	db, err := postgresstore.NewRunnerJournal(ctx, recoveryURL())
	if err != nil {
		t.Fatal(err)
	}
	f := newRunnerFixture(t)
	p := fixtureManifest(t, f)
	id := fmt.Sprintf("manifest-%d", time.Now().UnixNano())
	p.RunnerGroup = id
	p, err = grant.SignPlanManifest(ctx, f.signer, p)
	if err != nil {
		t.Fatal(err)
	}
	m := &ManifestContexts{Store: db, Issuer: p.Issuer, TenantID: p.TenantID, RunnerGroup: id, Keys: grant.StaticKeys{p.Issuer + "\x00grant-key": f.public}, Now: func() time.Time { return f.now }}
	if err := m.Pin(ctx, p); err != nil {
		t.Fatal(err)
	}
	db.Close()
	payload, _ := json.Marshal(p)
	for _, replica := range []string{"first", "restarted"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryProcessHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "THEMISY_RECOVERY_HELPER=none", "THEMISY_RECOVERY_ID="+id, "THEMISY_RECOVERY_REPLICA="+replica, "THEMISY_RECOVERY_MANIFEST="+string(payload), "THEMISY_RECOVERY_MANIFEST_KEY="+base64.RawURLEncoding.EncodeToString(f.public))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("manifest replica: %v %s", err, output)
		}
	}
	conn, err := pgx.Connect(ctx, recoveryURL())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var writes int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM recovery_test_writes WHERE test_id=$1`, id).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("writes=%d", writes)
	}
	db, err = postgresstore.NewRunnerJournal(ctx, recoveryURL())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m.Store = db
	next := p
	next.Revision = 2
	next.PlanHash = testDigest("b")
	next.ApprovalProofs = []string{"approval-2"}
	next, err = grant.SignPlanManifest(ctx, f.signer, next)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Pin(ctx, next); err != nil {
		t.Fatal(err)
	}
	// A reservation made for the old revision cannot dispatch after activation.
	r := store.RunnerActionRecord{TenantID: p.TenantID, RunnerGroup: id, RunID: p.RunID, StepID: p.StepID, GrantID: id + "-old", Nonce: "old-reserved", IdempotencyKey: "old-reserved", RequestHash: "old-hash", RunnerID: "old-runner", PlanRevision: 1, PlanHash: p.PlanHash, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	r, _, err = db.ReserveRunnerAction(ctx, r, domain.AuditEvent{ID: id + "-reserve", CorrelationID: id, Timestamp: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	r.Status = store.RunnerActionDispatched
	if err = db.CompleteRunnerAction(ctx, r, r.StateVersion, domain.AuditEvent{ID: id + "-dispatch", CorrelationID: id, Timestamp: time.Now()}); err != store.ErrConflict {
		t.Fatalf("old revision dispatched: %v", err)
	}
	if err = m.Pin(ctx, p); err == nil {
		t.Fatal("persistent revision rollback accepted")
	}
	fresh, err := db.GetManifest(ctx, p.TenantID, id, p.RunID, p.StepID, p.Action.Capability)
	if err != nil || fresh.Revision != 2 {
		t.Fatalf("manifest=%#v err=%v", fresh, err)
	}
}

func TestUnavailableSharedJournalAcquiresNoCredentials(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	ctx := context.Background()
	if err := postgresstore.MigrateRunner(ctx, recoveryURL()); err != nil {
		t.Fatal(err)
	}
	journal, err := postgresstore.NewRunnerJournal(ctx, recoveryURL())
	if err != nil {
		t.Fatal(err)
	}
	journal.Close()
	f := newRunnerFixture(t)
	f.runner.Journal = journal
	if _, err = f.runner.Execute(ctx, f.grant); err == nil {
		t.Fatal("unavailable DB accepted")
	}
	if f.credentials.Calls() != 0 || f.adapter.Calls() != 0 {
		t.Fatal("DB failure bypassed reservation")
	}
}

type testGrantVerifier struct{ fixture *runnerFixture }

func (v *testGrantVerifier) Verify(ctx context.Context, g protocol.ActionGrant) error {
	verifier := grant.Verifier{Issuer: g.Issuer, TenantID: g.TenantID, RunnerGroup: v.fixture.grant.RunnerGroup, Keys: grant.StaticKeys{g.Issuer + "\x00grant-key": v.fixture.public}, Now: func() time.Time { return v.fixture.now }}
	return verifier.Verify(ctx, g)
}

type crashJournal struct {
	store.RunnerJournal
	mode string
}

func (j *crashJournal) CompleteRunnerAction(ctx context.Context, r store.RunnerActionRecord, v int64, a domain.AuditEvent) error {
	if j.mode == "after-response" && r.Status == store.RunnerActionSucceeded {
		os.Exit(42)
	}
	return j.RunnerJournal.CompleteRunnerAction(ctx, r, v, a)
}

func (j *crashJournal) ReserveRunnerAction(ctx context.Context, r store.RunnerActionRecord, a domain.AuditEvent) (store.RunnerActionRecord, bool, error) {
	r, created, err := j.RunnerJournal.ReserveRunnerAction(ctx, r, a)
	if err == nil && created && j.mode == "reserved" {
		os.Exit(42)
	}
	return r, created, err
}

type processProvider struct{ mode, id string }

func (p *processProvider) Authorize(protocol.Target, protocol.Action) error { return nil }
func (p *processProvider) Execute(ctx context.Context, _ AdapterRequest, _ Credential) (AdapterResult, error) {
	if p.mode == "before-write" {
		os.Exit(42)
	}
	conn, err := pgx.Connect(ctx, recoveryURL())
	if err != nil {
		return AdapterResult{}, err
	}
	defer conn.Close(ctx)
	if _, err = conn.Exec(ctx, `INSERT INTO recovery_test_writes VALUES($1)`, p.id); err != nil {
		return AdapterResult{}, err
	}
	if p.mode == "after-write" {
		os.Exit(42)
	}
	return AdapterResult{ExternalExecutionID: p.id, CompletedAt: time.Now().UTC()}, nil
}
