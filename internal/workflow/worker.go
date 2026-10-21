package workflow

import (
	"context"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const DefaultTaskQueue = "themisy-releases"

func NewWorker(temporalClient client.Client, taskQueue string, activities *Activities) worker.Worker {
	if taskQueue == "" {
		taskQueue = DefaultTaskQueue
	}
	result := worker.New(temporalClient, taskQueue, worker.Options{})
	result.RegisterWorkflow(ReleaseWorkflow)
	result.RegisterActivity(activities.PersistRun)
	result.RegisterActivity(activities.EvaluateStep)
	result.RegisterActivity(activities.Deploy)
	result.RegisterActivity(activities.Verify)
	result.RegisterActivity(activities.Rollback)
	result.RegisterActivity(activities.AcquireSchedule)
	result.RegisterActivity(activities.CompleteSchedule)
	result.RegisterActivity(activities.RecoverExecution)
	return result
}

func RunWorker(ctx context.Context, temporalClient client.Client, taskQueue string, activities *Activities) error {
	return NewWorker(temporalClient, taskQueue, activities).Run(worker.InterruptCh())
}
