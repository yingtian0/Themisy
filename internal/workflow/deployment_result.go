package workflow

import (
	"encoding/json"
	"errors"

	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/store"

	"go.temporal.io/sdk/temporal"
)

// Version 1 stores the entire typed return value, bound to its durable execution.
// Legacy timestamp-only results cannot be reconstructed from retry arguments.
type deploymentResult struct {
	Version     int                 `json:"version"`
	ExecutionID string              `json:"execution_id"`
	Deployment  executor.Deployment `json:"deployment"`
}

func resultUnknown(err error) error {
	return temporal.NewNonRetryableApplicationError("execution result requires reconciliation", string(ErrorUnknownExternalState), err)
}

func validateDeploymentBinding(run domain.ReleaseRun, record store.ExecutionRecord, deployment executor.Deployment) error {
	if record.ID == "" || record.RunID != run.ID || record.Service != deployment.Service ||
		deployment.RunID != record.RunID || deployment.Environment != string(run.Environment) ||
		deployment.IdempotencyKey == "" || deployment.IdempotencyKey != record.IdempotencyKey ||
		deployment.ExternalID == "" || deployment.ExternalID != record.ExternalExecutionID ||
		deployment.StartedAt.IsZero() || deployment.FinishedAt.IsZero() || deployment.FinishedAt.Before(deployment.StartedAt) {
		return errors.New("deployment does not match durable execution")
	}
	matched := 0
	for _, step := range run.Steps {
		if step.Service == record.Service && step.Change.DesiredVersion == deployment.DesiredVersion && deployment.DesiredVersion != "" {
			matched++
		}
	}
	if matched != 1 {
		return errors.New("deployment does not match durable run step")
	}
	return nil
}

func encodeDeploymentResult(run domain.ReleaseRun, record store.ExecutionRecord, deployment executor.Deployment) (map[string]any, error) {
	if err := validateDeploymentBinding(run, record, deployment); err != nil {
		return nil, err
	}
	data, err := json.Marshal(deploymentResult{Version: 1, ExecutionID: record.ID, Deployment: deployment})
	if err != nil {
		return nil, err
	}
	return map[string]any{"deployment_result": string(data)}, nil
}

func restoreDeploymentResult(run domain.ReleaseRun, record store.ExecutionRecord, expected store.ExecutionRecord) (DeployResult, error) {
	if record.Status != store.ExecutionSucceeded || record.RunID != expected.RunID || record.Service != expected.Service || record.Adapter != expected.Adapter || record.IdempotencyKey != expected.IdempotencyKey {
		return DeployResult{}, resultUnknown(errors.New("execution binding or status mismatch"))
	}
	data, ok := record.Payload["deployment_result"].(string)
	if !ok {
		return DeployResult{}, resultUnknown(errors.New("legacy or missing deployment result"))
	}
	var result deploymentResult
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		return DeployResult{}, resultUnknown(err)
	}
	if result.Version != 1 || result.ExecutionID != record.ID {
		return DeployResult{}, resultUnknown(errors.New("unsupported deployment result version or execution identity"))
	}
	if err := validateDeploymentBinding(run, record, result.Deployment); err != nil {
		return DeployResult{}, resultUnknown(err)
	}
	d := result.Deployment
	return DeployResult{Execution: domain.Execution{ID: record.ID, Adapter: record.Adapter, IdempotencyKey: record.IdempotencyKey, Status: string(record.Status), ExternalExecutionID: record.ExternalExecutionID, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt}, Deployment: d}, nil
}
