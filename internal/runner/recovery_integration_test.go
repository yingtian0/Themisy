//go:build integration

package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"os"
	"os/exec"
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
	_, _ = f.runner.Execute(ctx, f.grant)
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
