//go:build integration

package workflow_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	wf "themisy/internal/workflow"
	"themisy/pkg/adapter"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// This helper only runs when launched by the parent test binary. It is not a
// production Worker entry point or a configurable command execution feature.
func TestRecoveryWorkerProcess(t *testing.T) {
	mode := os.Getenv("THEMISY_RECOVERY_TEST_PROCESS")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	if mode != "first" && mode != "replacement" {
		t.Fatal("unknown subprocess mode")
	}
	url, address, queue := os.Getenv("THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL"), os.Getenv("THEMISY_TEMPORAL_ADDRESS"), os.Getenv("THEMISY_RECOVERY_TEST_QUEUE")
	if url == "" || address == "" || queue == "" {
		t.Fatal("missing dedicated subprocess configuration")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	st, err := postgresstore.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tc, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	rollback := os.Getenv("THEMISY_RECOVERY_TEST_ROLLBACK") == "1"
	var verification executor.ReleaseExecutor = executor.NewMock(nil)
	if rollback {
		verification = rollbackPathVerifier{ReleaseExecutor: verification, failed: executor.NewMock(map[string]executor.MockBehavior{"": {VerifyStatus: adapter.VerificationFail}})}
	}
	forbidden := &forbidWriteExecutor{ReleaseExecutor: verification}
	var execution executor.ReleaseExecutor = forbidden
	guarded := &forbidNewRecoveryGrantStore{DurableStore: st}
	durableRun, err := st.GetRun(queue)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "replacement" && len(durableRun.Steps) > 1 {
		signer, _, err := grant.NewDevelopmentSigner("grant-key")
		if err != nil {
			t.Fatal(err)
		}
		issuer, err := application.NewGrants(guarded, signer, "https://control.example", 2*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		execution = &application.GrantExecutor{Grants: issuer, Verification: verification, PollInterval: 10 * time.Millisecond}
	}
	if mode == "first" {
		signer, _, err := grant.NewDevelopmentSigner("grant-key")
		if err != nil {
			t.Fatal(err)
		}
		ttl := 2 * time.Minute
		run, err := st.GetRun(queue)
		if err != nil {
			t.Fatal(err)
		}
		if !run.Plan.ExpiresAt.IsZero() {
			ttl = 2 * time.Second
		}
		issuer, err := application.NewGrants(st, signer, "https://control.example", ttl)
		if err != nil {
			t.Fatal(err)
		}
		execution = &shortGrantWaitExecutor{pendingRollback: rollback, ReleaseExecutor: &application.GrantExecutor{Grants: issuer, Verification: verification, PollInterval: 10 * time.Millisecond}}
	}
	a := wf.NewActivities(st, policy.New(), execution)
	w := worker.New(tc, queue, worker.Options{})
	w.RegisterWorkflow(wf.ReleaseWorkflow)
	w.RegisterActivity(a.PersistRun)
	w.RegisterActivity(a.EvaluateStep)
	w.RegisterActivity(a.AcquireSchedule)
	w.RegisterActivity(a.CompleteSchedule)
	w.RegisterActivity(a.Deploy)
	w.RegisterActivity(a.Verify)
	w.RegisterActivity(a.Rollback)
	if mode == "first" {
		w.RegisterActivityWithOptions(func(ctx context.Context, in wf.RecoverExecutionInput) (wf.RecoverExecutionResult, error) {
			result, err := a.RecoverExecution(ctx, in)
			if err != nil || !result.Supported || result.Ready {
				return result, err
			}
			if err := os.WriteFile(os.Getenv("THEMISY_RECOVERY_TEST_READY_FILE"), []byte("pending"), 0600); err != nil {
				return wf.RecoverExecutionResult{}, err
			}
			return wf.RecoverExecutionResult{}, activity.ErrResultPending
		}, activity.RegisterOptions{Name: wf.ActivityRecoverExecution})
	} else {
		w.RegisterActivity(a.RecoverExecution)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Minute):
		t.Fatal("parent did not stop child Worker")
	}
	if guarded.calls.Load() != 0 {
		t.Fatal("replacement attempted a new Grant after Plan expiry")
	}
	if forbidden.calls.Load() != 0 {
		t.Fatalf("replacement dispatched %d writes", forbidden.calls.Load())
	}
}

type recoveryProcessWorker struct {
	t       *testing.T
	command *exec.Cmd
	crash   bool
}

func (p *recoveryProcessWorker) Stop() {
	if p.crash {
		if err := p.command.Process.Kill(); err != nil {
			p.t.Errorf("kill first Worker process: %v", err)
		}
		if err := p.command.Wait(); err == nil {
			p.t.Error("first Worker exited without expected forced kill")
		}
	} else {
		if err := p.command.Process.Signal(os.Interrupt); err != nil {
			p.t.Errorf("stop replacement Worker: %v", err)
		}
		if err := p.command.Wait(); err != nil {
			p.t.Errorf("replacement Worker failed: %v", err)
		}
	}
}
func startRecoveryWorkerProcess(t *testing.T, ctx context.Context, queue string, rollback, first bool, ready chan<- struct{}) *recoveryProcessWorker {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	readyPath := filepath.Join(directory, "ready")
	output, err := os.Create(filepath.Join(directory, "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	mode := "replacement"
	if first {
		mode = "first"
	}
	rollbackMode := "0"
	if rollback {
		rollbackMode = "1"
	}
	command := exec.Command(executable, "-test.run=^TestRecoveryWorkerProcess$", "-test.v")
	command.Env = append(os.Environ(), "THEMISY_RECOVERY_TEST_PROCESS="+mode, "THEMISY_RECOVERY_TEST_QUEUE="+queue, "THEMISY_RECOVERY_TEST_READY_FILE="+readyPath, "THEMISY_RECOVERY_TEST_ROLLBACK="+rollbackMode)
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("started %s Worker OS process %d", mode, command.Process.Pid)
	if first {
		go func() {
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(readyPath); err == nil {
						select {
						case ready <- struct{}{}:
						case <-ctx.Done():
						}
						return
					}
				}
			}
		}()
	}
	return &recoveryProcessWorker{t: t, command: command, crash: first}
}

func TestTemporalSignedRecoveryAcrossProcesses(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%t", rollback), func(t *testing.T) { testTemporalSignedRecoveryWithProcesses(t, rollback, true) })
	}
}

func TestTemporalSignedRecoveryCancelAcrossProcesses(t *testing.T) {
	testTemporalSignedRecoveryScenario(t, false, true, true)
}

// The real Grants.Issue must reject expired Plan context before reaching this
// persistence boundary. The guard also prevents a fixture bug from issuing.
type forbidNewRecoveryGrantStore struct {
	store.DurableStore
	calls atomic.Int32
}

func (s *forbidNewRecoveryGrantStore) CreateGrantDispatch(context.Context, store.GrantDispatchRecord, domain.AuditEvent, domain.OutboxEvent) (store.GrantDispatchRecord, bool, error) {
	s.calls.Add(1)
	return store.GrantDispatchRecord{}, false, fmt.Errorf("new Grant forbidden in expired recovery fixture")
}
func TestTemporalExpiredRecoveryCannotAuthorizeNextStep(t *testing.T) {
	testTemporalSignedRecoveryOptions(t, signedRecoveryOptions{separateProcesses: true, nextStepAfterExpiry: true})
}
