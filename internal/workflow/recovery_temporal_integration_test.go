//go:build integration

package workflow

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/policy"
	postgresstore "themisy/internal/store/postgres"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
)

// This tests real Workflow history and durable Run persistence. The Grant
// recovery source and provider are simulated; it is not full Runner recovery.
func TestTemporalUnknownWaitCancelAcrossWorkersAndReplay(t *testing.T) {
	url, address := os.Getenv("THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL"), os.Getenv("THEMISY_TEMPORAL_ADDRESS")
	if url == "" || address == "" {
		t.Skip("requires explicitly dedicated PostgreSQL and Temporal")
	}
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	tc, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	for _, rollback := range []bool{false, true} {
		name := "deploy"
		if rollback {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			st, err := postgresstore.New(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			run, _, exec := workflowFixture(t, false, nil)
			run.ID += "-live-" + name + "-" + time.Now().UTC().Format("20060102150405.000000000")
			run.WorkflowID, run.RequestID = run.ID, run.ID
			if _, _, err := st.CreateRun(&run); err != nil {
				t.Fatal(err)
			}
			// Buffered notification means the Activity can complete before Worker stop.
			recoveryRead := make(chan struct{}, 10)
			var rollbackCalls atomic.Int32
			startWorker := func(a *Activities) worker.Worker {
				w := worker.New(tc, run.ID, worker.Options{})
				w.RegisterWorkflow(ReleaseWorkflow)
				w.RegisterActivity(a.PersistRun)
				w.RegisterActivity(a.EvaluateStep)
				w.RegisterActivity(a.AcquireSchedule)
				w.RegisterActivity(a.CompleteSchedule)
				w.RegisterActivity(a.Deploy)
				w.RegisterActivityWithOptions(func(context.Context, RollbackInput) (RollbackResult, error) {
					rollbackCalls.Add(1)
					return RollbackResult{}, temporal.NewNonRetryableApplicationError("rollback unresolved", "UNKNOWN_EXTERNAL_STATE", nil)
				}, activity.RegisterOptions{Name: ActivityRollback})
				w.RegisterActivityWithOptions(func(context.Context, VerifyInput) (executor.VerificationResult, error) {
					return executor.VerificationResult{Status: "FAIL"}, nil
				}, activity.RegisterOptions{Name: ActivityVerify})
				w.RegisterActivityWithOptions(func(context.Context, RecoverExecutionInput) (RecoverExecutionResult, error) {
					recoveryRead <- struct{}{}
					return RecoverExecutionResult{Supported: true}, nil
				}, activity.RegisterOptions{Name: ActivityRecoverExecution})
				if err := w.Start(); err != nil {
					t.Fatal(err)
				}
				return w
			}
			var e executor.ReleaseExecutor = exec
			if !rollback {
				e = pendingGrantExecutor{ReleaseExecutor: exec, grantID: run.ID + "-existing-grant"}
			}
			// The coordinator is intentionally in-process in this fixture. #50's
			// two-process fenced coordinator acceptance remains separate.
			first := startWorker(NewActivities(durableStoreNoCoordinator{DurableStore: st}, policy.New(), e))
			stopped := false
			defer func() {
				if !stopped {
					first.Stop()
				}
			}()
			execution, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: run.ID, TaskQueue: run.ID}, ReleaseWorkflow, ReleaseInput{Run: run})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-recoveryRead:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// A query round-trip ensures the Workflow has processed pending recovery.
			state, err := tc.QueryWorkflow(ctx, run.ID, execution.GetRunID(), QueryState)
			if err != nil {
				t.Fatal(err)
			}
			var waiting domain.ReleaseRun
			if err := state.Get(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting.Status != domain.RunBlocked || waiting.Steps[0].Status != domain.StepUnknown {
				t.Fatalf("not waiting: %s/%s", waiting.Status, waiting.Steps[0].Status)
			}
			first.Stop()
			stopped = true
			replacement, err := postgresstore.New(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			forbidden := &forbidWriteExecutor{ReleaseExecutor: exec}
			second := startWorker(NewActivities(durableStoreNoCoordinator{DurableStore: replacement}, policy.New(), forbidden))
			defer second.Stop()
			if err := tc.SignalWorkflow(ctx, run.ID, execution.GetRunID(), SignalCancel, ControlSignal{Actor: "operator"}); err != nil {
				t.Fatal(err)
			}
			var result domain.ReleaseRun
			if err := execution.Get(ctx, &result); err != nil {
				t.Fatal(err)
			}
			saved, err := replacement.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []domain.ReleaseRun{result, *saved} {
				if value.Status != domain.RunCancelled || value.Steps[0].Status != domain.StepUnknown {
					t.Fatalf("cancel overwrote unresolved state: %s/%s", value.Status, value.Steps[0].Status)
				}
			}
			wantWrites := 0
			if rollback {
				wantWrites = 1
			}
			if calls := exec.DeployCalls(run.ID + "/staging/identity/sha-1"); calls != wantWrites {
				t.Fatalf("deploy calls=%d want=%d", calls, wantWrites)
			}
			if forbidden.calls != 0 || rollbackCalls.Load() != int32(wantWrites) {
				t.Fatalf("unexpected repeated Write: replacement=%d rollback=%d", forbidden.calls, rollbackCalls.Load())
			}
			history := &historypb.History{}
			iterator := tc.GetWorkflowHistory(ctx, run.ID, execution.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
			for iterator.HasNext() {
				event, err := iterator.Next()
				if err != nil {
					t.Fatal(err)
				}
				history.Events = append(history.Events, event)
			}
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(ReleaseWorkflow)
			if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
				t.Fatal(err)
			}
		})
	}
}
