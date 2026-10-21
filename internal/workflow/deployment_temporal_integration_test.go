//go:build integration

package workflow

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"themisy/internal/executor"
	"themisy/internal/policy"
	"themisy/internal/scheduler"
	postgresstore "themisy/internal/store/postgres"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// ErrResultPending deliberately suppresses Activity completion after persistence;
// the server must time out the attempt and dispatch its retry to a new Worker.
type droppedDeploymentCompletion struct {
	activities *Activities
	committed  chan DeployResult
	rollback   bool
}

func (d *droppedDeploymentCompletion) Deploy(ctx context.Context, in DeployInput) (DeployResult, error) {
	result, err := d.activities.Deploy(ctx, in)
	if err != nil {
		return result, err
	}
	if !d.rollback {
		d.committed <- result
		return DeployResult{}, activity.ErrResultPending
	}
	return result, nil
}

func (d *droppedDeploymentCompletion) Rollback(ctx context.Context, in RollbackInput) (RollbackResult, error) {
	result, err := d.activities.Rollback(ctx, in)
	if err != nil {
		return result, err
	}
	if d.rollback {
		d.committed <- DeployResult(result)
		return RollbackResult{}, activity.ErrResultPending
	}
	return result, nil
}

type restoredOperationResults struct {
	Deploy   DeployResult
	Rollback RollbackResult
}

type deploymentRetryInput struct {
	Deploy   DeployInput
	Schedule scheduler.Step
}

func deploymentRetryWorkflow(ctx temporalworkflow.Context, fixture deploymentRetryInput) (restoredOperationResults, error) {
	ctx = temporalworkflow.WithActivityOptions(ctx, temporalworkflow.ActivityOptions{StartToCloseTimeout: 2 * time.Second, ScheduleToCloseTimeout: 45 * time.Second, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumAttempts: 5}})
	var result restoredOperationResults
	input := fixture.Deploy
	var scheduled ScheduleResult
	if err := temporalworkflow.ExecuteActivity(ctx, ActivityAcquireSchedule, ScheduleInput{RunID: input.RunID, Step: fixture.Schedule}).Get(ctx, &scheduled); err != nil {
		return result, err
	}
	if !scheduled.Allowed || scheduled.Lease == nil {
		return result, temporal.NewNonRetryableApplicationError("fixture deploy reservation refused", "FIXTURE_SCHEDULE_UNAVAILABLE", nil)
	}
	input.ScheduleLease = scheduled.Lease
	if err := temporalworkflow.ExecuteActivity(ctx, ActivityDeploy, input).Get(ctx, &result.Deploy); err != nil {
		return result, err
	}
	if err := temporalworkflow.ExecuteActivity(ctx, ActivityCompleteSchedule, ScheduleCompleteInput{RunID: input.RunID, StepID: fixture.Schedule.ID, Lease: scheduled.Lease}).Get(ctx, nil); err != nil {
		return result, err
	}
	rollbackStep := fixture.Schedule
	rollbackStep.ID += "/rollback"
	if err := temporalworkflow.ExecuteActivity(ctx, ActivityAcquireSchedule, ScheduleInput{RunID: input.RunID, Step: rollbackStep}).Get(ctx, &scheduled); err != nil {
		return result, err
	}
	if !scheduled.Allowed || scheduled.Lease == nil {
		return result, temporal.NewNonRetryableApplicationError("fixture rollback reservation refused", "FIXTURE_SCHEDULE_UNAVAILABLE", nil)
	}
	input.ScheduleLease = scheduled.Lease
	if err := temporalworkflow.ExecuteActivity(ctx, ActivityRollback, RollbackInput{Deploy: input, Deployment: result.Deploy.Deployment}).Get(ctx, &result.Rollback); err != nil {
		return result, err
	}
	if err := temporalworkflow.ExecuteActivity(ctx, ActivityCompleteSchedule, ScheduleCompleteInput{RunID: input.RunID, StepID: rollbackStep.ID, Lease: scheduled.Lease}).Get(ctx, nil); err != nil {
		return result, err
	}
	var verified executor.VerificationResult
	err := temporalworkflow.ExecuteActivity(ctx, ActivityVerify, VerifyInput{Deployment: result.Rollback.Deployment}).Get(ctx, &verified)
	return result, err
}

func TestTemporalDeploymentCompletionLossAcrossWorkers(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" && os.Getenv("THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL") == "" {
		t.Skip("requires dedicated PostgreSQL/Temporal environment")
	}
	url := os.Getenv("THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("THEMISY_DATABASE_URL")
	}
	if url == "" {
		url = "postgres://themisy:themisy@localhost:5432/themisy?sslmode=disable"
	}
	address := os.Getenv("THEMISY_TEMPORAL_ADDRESS")
	if address == "" {
		address = "localhost:7233"
	}
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	st, err := postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, _, exec := workflowFixture(t, false, nil)
	run.ID += "-temporal-" + time.Now().UTC().Format("20060102150405.000000000")
	run.RequestID, run.WorkflowID = run.ID, run.ID
	if _, _, err := st.CreateRun(&run); err != nil {
		t.Fatal(err)
	}
	tc, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	queue := run.ID
	counted := &countRollbackExecutor{ReleaseExecutor: exec}
	committed := make(chan DeployResult, 1)
	startWorker := func(drop *droppedDeploymentCompletion) worker.Worker {
		w := worker.New(tc, queue, worker.Options{})
		w.RegisterWorkflow(deploymentRetryWorkflow)
		a := NewActivities(st, policy.New(), &forbidWriteExecutor{ReleaseExecutor: exec})
		if drop != nil {
			a = drop.activities
			w.RegisterActivityWithOptions(drop.Deploy, activity.RegisterOptions{Name: ActivityDeploy})
			w.RegisterActivityWithOptions(drop.Rollback, activity.RegisterOptions{Name: ActivityRollback})
		} else {
			w.RegisterActivityWithOptions(a.Deploy, activity.RegisterOptions{Name: ActivityDeploy})
			w.RegisterActivityWithOptions(a.Rollback, activity.RegisterOptions{Name: ActivityRollback})
		}
		w.RegisterActivityWithOptions(a.Verify, activity.RegisterOptions{Name: ActivityVerify})
		w.RegisterActivityWithOptions(a.AcquireSchedule, activity.RegisterOptions{Name: ActivityAcquireSchedule})
		w.RegisterActivityWithOptions(a.CompleteSchedule, activity.RegisterOptions{Name: ActivityCompleteSchedule})
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		return w
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	input := DeployInput{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: run.ID + "/operation"}
	first := startWorker(&droppedDeploymentCompletion{activities: NewActivities(st, policy.New(), counted), committed: committed})
	fixture := deploymentRetryInput{Deploy: input, Schedule: schedulingStep(run, run.Steps[0])}
	if fixture.Schedule.ID != input.Service {
		first.Stop()
		t.Fatal("fixture schedule target differs from deployment")
	}
	execution, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: run.ID, TaskQueue: queue}, deploymentRetryWorkflow, fixture)
	if err != nil {
		first.Stop()
		t.Fatal(err)
	}
	var deployed, rolledBack DeployResult
	select {
	case deployed = <-committed:
	case <-ctx.Done():
		first.Stop()
		t.Fatal(ctx.Err())
	}
	first.Stop()
	second := startWorker(&droppedDeploymentCompletion{activities: NewActivities(st, policy.New(), counted), committed: committed, rollback: true})
	select {
	case rolledBack = <-committed:
	case <-ctx.Done():
		second.Stop()
		t.Fatal(ctx.Err())
	}
	second.Stop()
	third := startWorker(nil)
	defer third.Stop()
	var result restoredOperationResults
	if err := execution.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Deploy, deployed) || !reflect.DeepEqual(result.Rollback, RollbackResult(rolledBack)) {
		t.Fatalf("Activity retry changed results: %+v", result)
	}
	if exec.DeployCalls(input.IdempotencyKey) != 1 || counted.rollbacks != 1 {
		t.Fatal("duplicate Write across Worker restart")
	}
	audits, err := st.AuditEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range audits {
		counts[event.Action]++
	}
	for _, action := range []string{"schedule.acquire", "schedule.admit", "schedule.complete"} {
		if counts[action] != 2 {
			t.Fatalf("expected one durable reservation per deploy/rollback; %s=%d", action, counts[action])
		}
	}
}
