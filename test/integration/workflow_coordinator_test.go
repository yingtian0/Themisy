//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/scheduler"
	postgresstore "themisy/internal/store/postgres"

	"github.com/jackc/pgx/v5"
)

func TestPostgresWorkflowCoordinatorAcrossIndependentStores(t *testing.T) {
	requireDedicatedCoordinatorIntegration(t)
	ctx := context.Background()
	databaseURL := integrationDatabaseURL()
	if err := resetCoordinatorDatabase(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resetCoordinatorDatabase(ctx, databaseURL); err != nil {
			t.Errorf("clean dedicated coordinator database: %v", err)
		}
	})
	first, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	limits := scheduler.DefaultLimits()
	limits.TenantGlobal = 1
	limits.DefaultPerDimension = 1
	step := scheduler.Step{ID: "coordinator-shared-service", Tenant: "integration-tenant", Environment: "staging", Region: "region-a", Cluster: "cluster-a", Team: "team-a", RiskTier: "low", FailureDomains: []string{"failure-domain-a"}}
	constraints, err := scheduler.ConstraintsFor(step, limits)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	leases := make(chan scheduler.ScheduleLease, 2)
	blocked := make(chan []scheduler.Blocked, 2)
	errs := make(chan error, 2)
	stores := []scheduler.DurableCoordinator{first, second}
	var wg sync.WaitGroup
	for index, coordinator := range stores {
		wg.Add(1)
		go func(index int, coordinator scheduler.DurableCoordinator) {
			defer wg.Done()
			lease, held, _, acquireErr := coordinator.AcquireSchedule(ctx, acquireRequest(fmt.Sprintf("contention-%d", index), fmt.Sprintf("worker-run-%d", index), step, constraints, now, now.Add(10*time.Minute)))
			if acquireErr != nil {
				errs <- acquireErr
				return
			}
			if len(held) == 0 {
				leases <- lease
			} else {
				blocked <- held
			}
		}(index, coordinator)
	}
	wg.Wait()
	close(leases)
	close(blocked)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var firstLease scheduler.ScheduleLease
	leaseCount := 0
	for lease := range leases {
		firstLease = lease
		leaseCount++
	}
	blockedCount := 0
	for range blocked {
		blockedCount++
	}
	if leaseCount != 1 || blockedCount != 1 {
		t.Fatalf("lease count=%d blocked count=%d, want one each", leaseCount, blockedCount)
	}

	admission := scheduler.AdmissionRequest{Lease: firstLease, OperationKey: "operation-unknown", Now: now.Add(time.Second), Audit: coordinatorAudit("admit-unknown", firstLease.RunID, now.Add(time.Second))}
	if err := first.AdmitSchedule(ctx, admission); err != nil {
		t.Fatal(err)
	}
	unknown := scheduler.CompletionRequest{Lease: firstLease, Outcome: scheduler.ScheduleOutcomeUnknown, Circuit: scheduler.CircuitPolicy{MinimumSamples: 1, WindowSize: 4, ErrorRate: .5}, Now: now.Add(2 * time.Second), Audit: coordinatorAudit("complete-unknown", firstLease.RunID, now.Add(2*time.Second))}
	if _, err := second.CompleteSchedule(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	unknownRetry := acquireRequest("unknown-retry", firstLease.RunID+"-retry", step, constraints, now.Add(3*time.Second), now.Add(10*time.Minute))
	if _, held, _, err := second.AcquireSchedule(ctx, unknownRetry); err != nil || len(held) == 0 {
		t.Fatalf("UNKNOWN must retain shared capacity: blocked=%v err=%v", held, err)
	}

	expiredUnknownRetry := acquireRequest("expired-unknown-retry", "new-run-after-expiry", step, constraints, firstLease.ExpiresAt.Add(time.Second), firstLease.ExpiresAt.Add(time.Minute))
	if _, held, _, err := second.AcquireSchedule(ctx, expiredUnknownRetry); err != nil || len(held) == 0 {
		t.Fatalf("expired UNKNOWN must retain shared capacity: blocked=%v err=%v", held, err)
	}

	resolved := scheduler.CompletionRequest{Lease: firstLease, Outcome: scheduler.ScheduleOutcomeFailed, Circuit: unknown.Circuit, Now: firstLease.ExpiresAt.Add(time.Second), Audit: coordinatorAudit("resolve-unknown", firstLease.RunID, firstLease.ExpiresAt.Add(time.Second))}
	state, err := first.CompleteSchedule(ctx, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if state.Open {
		t.Fatal("one failed sample should not open the breaker at 50% threshold")
	}
	if _, err := second.CompleteSchedule(ctx, resolved); err != nil {
		t.Fatalf("duplicate same completion must be idempotent: %v", err)
	}
	productionStep := step
	productionStep.ID = "coordinator-breaker-service"
	productionStep.Environment = "production"
	productionConstraints, err := scheduler.ConstraintsFor(productionStep, limits)
	if err != nil {
		t.Fatal(err)
	}
	productionRequest := acquireRequest("production-failure", "production-failure", productionStep, productionConstraints, now, now.Add(10*time.Minute))
	productionPolicy := scheduler.CircuitPolicy{MinimumSamples: 2, WindowSize: 4, ErrorRate: .5}
	productionRequest.Circuit = productionPolicy
	productionLease, held, _, err := first.AcquireSchedule(ctx, productionRequest)
	if err != nil || len(held) != 0 {
		t.Fatalf("production acquire lease=%#v held=%v err=%v", productionLease, held, err)
	}
	if err := second.AdmitSchedule(ctx, scheduler.AdmissionRequest{Lease: productionLease, OperationKey: "production-op", Now: now.Add(time.Second), Audit: coordinatorAudit("production-admit", productionLease.RunID, now.Add(time.Second))}); err != nil {
		t.Fatalf("production admission lease=%#v: %v", productionLease, err)
	}
	productionState, err := first.CompleteSchedule(ctx, scheduler.CompletionRequest{Lease: productionLease, Outcome: scheduler.ScheduleOutcomeFailed, Circuit: productionPolicy, Now: now.Add(2 * time.Second), Audit: coordinatorAudit("production-failed", productionLease.RunID, now.Add(2*time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	if productionState.Open {
		t.Fatalf("production circuit did not open: %#v", productionState)
	}
	if _, err := second.CompleteSchedule(ctx, scheduler.CompletionRequest{Lease: productionLease, Outcome: scheduler.ScheduleOutcomeFailed, Circuit: productionPolicy, Now: now.Add(3 * time.Second), Audit: coordinatorAudit("production-failed-duplicate", productionLease.RunID, now.Add(3*time.Second))}); err != nil {
		t.Fatalf("duplicate same completion must be idempotent: %v", err)
	}
	restarted, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	persistedCircuit, err := restarted.GetCircuitState(ctx, "integration-tenant")
	if err != nil {
		t.Fatal(err)
	}
	if persistedCircuit.Open || persistedCircuit.Samples != 1 || persistedCircuit.Failures != 1 {
		t.Fatalf("breaker did not survive Store restart: %#v", persistedCircuit)
	}
	if _, err := second.CompleteSchedule(ctx, scheduler.CompletionRequest{Lease: firstLease, Outcome: scheduler.ScheduleOutcomeSucceeded, Circuit: unknown.Circuit, Now: now.Add(4 * time.Second), Audit: coordinatorAudit("resolve-unknown-conflict", firstLease.RunID, now.Add(4*time.Second))}); !errors.Is(err, scheduler.ErrUnresolved) {
		t.Fatalf("conflicting duplicate resolution error=%v, want unresolved", err)
	}
	secondProductionRequest := acquireRequest("production-failure-2", "production-failure-2", productionStep, productionConstraints, now.Add(4*time.Second), now.Add(8*time.Second))
	secondProductionRequest.Circuit = productionPolicy
	secondProductionLease, held, _, err := second.AcquireSchedule(ctx, secondProductionRequest)
	if err != nil || len(held) != 0 {
		t.Fatalf("second production acquire held=%v err=%v", held, err)
	}
	if err := restarted.AdmitSchedule(ctx, scheduler.AdmissionRequest{Lease: secondProductionLease, OperationKey: "production-op-2", Now: now.Add(5 * time.Second), Audit: coordinatorAudit("production-admit-2", secondProductionLease.RunID, now.Add(5*time.Second))}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.AdmitSchedule(ctx, scheduler.AdmissionRequest{Lease: secondProductionLease, OperationKey: "production-op-2", Now: now.Add(9 * time.Second), Audit: coordinatorAudit("production-admit-2-retry", secondProductionLease.RunID, now.Add(9*time.Second))}); !errors.Is(err, scheduler.ErrLeaseConflict) {
		t.Fatalf("expired admitted lease error=%v, want lease conflict", err)
	}
	opened, err := restarted.CompleteSchedule(ctx, scheduler.CompletionRequest{Lease: secondProductionLease, Outcome: scheduler.ScheduleOutcomeFailed, Circuit: productionPolicy, Now: now.Add(7 * time.Second), Audit: coordinatorAudit("production-failed-2", secondProductionLease.RunID, now.Add(7*time.Second))})
	if err != nil || !opened.Open {
		t.Fatalf("second failure should open breaker: state=%#v err=%v", opened, err)
	}
	blockedRequest := acquireRequest("breaker-block", "production-blocked", productionStep, productionConstraints, now.Add(7*time.Second), now.Add(10*time.Minute))
	blockedRequest.Circuit = productionPolicy
	if _, held, state, err := restarted.AcquireSchedule(ctx, blockedRequest); err != nil || !state.Open || len(held) == 0 {
		t.Fatalf("open production circuit should block: blocked=%v state=%#v err=%v", held, state, err)
	}

	afterRestart, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer afterRestart.Close()
	persistedOpen, err := afterRestart.GetCircuitState(ctx, "integration-tenant")
	if err != nil || !persistedOpen.Open || persistedOpen.Samples != 2 || persistedOpen.Failures != 2 {
		t.Fatalf("open breaker lost after restart: state=%#v err=%v", persistedOpen, err)
	}
	blockedAfterRestart := acquireRequest("breaker-block-after-restart", "production-blocked-after-restart", productionStep, productionConstraints, now.Add(8*time.Second), now.Add(10*time.Minute))
	blockedAfterRestart.Circuit = productionPolicy
	if _, held, state, err := afterRestart.AcquireSchedule(ctx, blockedAfterRestart); err != nil || !state.Open || len(held) == 0 {
		t.Fatalf("restarted Store must preserve open breaker refusal: blocked=%v state=%#v err=%v", held, state, err)
	}

	// A stale owner token/fence cannot admit a provider write.
	stale := firstLease
	stale.Fence++
	if err := second.AdmitSchedule(ctx, scheduler.AdmissionRequest{Lease: stale, OperationKey: "stale-operation", Now: now.Add(5 * time.Second), Audit: coordinatorAudit("stale-admit", stale.RunID, now.Add(5*time.Second))}); !errors.Is(err, scheduler.ErrLeaseConflict) {
		t.Fatalf("stale fencing value error=%v, want lease conflict", err)
	}
}

func TestPostgresWorkflowCoordinatorSeparateProcesses(t *testing.T) {
	requireDedicatedCoordinatorIntegration(t)
	ctx := context.Background()
	databaseURL := integrationDatabaseURL()
	if err := resetCoordinatorDatabase(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resetCoordinatorDatabase(ctx, databaseURL); err != nil {
			t.Errorf("clean dedicated coordinator database: %v", err)
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 2)
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	for index := 0; index < 2; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			command := exec.Command(executable, "-test.run=^TestCoordinatorWorkerProcess$")
			command.Env = append(os.Environ(), "THEMISY_COORDINATOR_WORKER_PROCESS=1", "THEMISY_WORKER_ID="+fmt.Sprint(index), "THEMISY_DATABASE_URL="+databaseURL)
			output, err := command.CombinedOutput()
			if err != nil {
				errorsFound <- fmt.Errorf("worker process %d: %w: %s", index, err, output)
				return
			}
			for _, line := range strings.Split(string(output), "\n") {
				if strings.HasPrefix(line, "COORDINATOR_RESULT=") {
					results <- strings.TrimPrefix(line, "COORDINATOR_RESULT=")
					return
				}
			}
			errorsFound <- fmt.Errorf("worker process %d returned no result: %s", index, output)
		}(index)
	}
	workers.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	var acquired, blocked int
	for result := range results {
		switch result {
		case "acquired":
			acquired++
		case "blocked":
			blocked++
		default:
			t.Fatalf("unexpected worker result %q", result)
		}
	}
	if acquired != 1 || blocked != 1 {
		t.Fatalf("separate worker processes acquired=%d blocked=%d, want one each", acquired, blocked)
	}
}

func TestCoordinatorWorkerProcess(t *testing.T) {
	if os.Getenv("THEMISY_COORDINATOR_WORKER_PROCESS") != "1" {
		t.Skip("helper process for TestPostgresWorkflowCoordinatorSeparateProcesses")
	}
	ctx := context.Background()
	coordinator, err := postgresstore.New(ctx, integrationDatabaseURL())
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	limits := scheduler.DefaultLimits()
	limits.TenantGlobal = 1
	limits.DefaultPerDimension = 1
	step := scheduler.Step{ID: "coordinator-process-service", Tenant: "integration-tenant", Environment: "staging", Region: "region-a", Cluster: "cluster-a", Team: "team-a", RiskTier: "low", FailureDomains: []string{"failure-domain-a"}}
	constraints, err := scheduler.ConstraintsFor(step, limits)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	workerID := os.Getenv("THEMISY_WORKER_ID")
	_, blocked, _, err := coordinator.AcquireSchedule(ctx, acquireRequest("process-"+workerID, "process-run-"+workerID, step, constraints, now, now.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) > 0 {
		_, _ = os.Stdout.WriteString("COORDINATOR_RESULT=blocked\n")
		return
	}
	_, _ = os.Stdout.WriteString("COORDINATOR_RESULT=acquired\n")
}

func resetCoordinatorDatabase(ctx context.Context, databaseURL string) error {
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer connection.Close(ctx)
	var database string
	if err := connection.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return err
	}
	if database != "themisy" {
		return fmt.Errorf("unexpected integration database %q", database)
	}
	if _, err := connection.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		return fmt.Errorf("reset only dedicated integration database: %w", err)
	}
	return postgresstore.Migrate(databaseURL, false)
}

func acquireRequest(id, runID string, step scheduler.Step, constraints []scheduler.Constraint, now, until time.Time) scheduler.AcquireRequest {
	return scheduler.AcquireRequest{ReservationID: id, RunID: runID, Step: step, Constraints: constraints, Circuit: scheduler.CircuitPolicy{MinimumSamples: 2, WindowSize: 4, ErrorRate: .5}, OwnerToken: "owner-" + id, Now: now, LeaseUntil: until, Audit: coordinatorAudit("audit-"+id, runID, now)}
}

func coordinatorAudit(id, runID string, at time.Time) domain.AuditEvent {
	return domain.AuditEvent{ID: id, CorrelationID: runID, ActorType: "system", ActorID: "integration", Action: "integration.coordinator", ResourceType: "release_step", ResourceID: "coordinator", Result: "OK", Timestamp: at}
}

func requireDedicatedCoordinatorIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("THEMISY_COORDINATOR_INTEGRATION") != "1" {
		t.Skip("set THEMISY_COORDINATOR_INTEGRATION=1 with a dedicated disposable PostgreSQL database URL")
	}
	requireIntegration(t)
	parsedURL := integrationDatabaseURL()
	parsed, err := url.Parse(parsedURL)
	if err != nil || parsed.Host != "127.0.0.1:56432" || parsed.Path != "/themisy" {
		t.Fatalf("refusing coordinator integration without the dedicated test port 56432; set THEMISY_DATABASE_URL explicitly")
	}
}
