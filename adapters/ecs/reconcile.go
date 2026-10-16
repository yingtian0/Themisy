package ecs

import (
	"context"
	"errors"
	"slices"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

func (a *Adapter) Reconcile(ctx context.Context, request adapter.ReconcileRequest, credential credentials.Credential) (adapter.ReconcileResult, error) {
	target, desired, pending, err := a.reconcileTarget(request)
	if err != nil {
		return adapter.ReconcileResult{}, terminal("reconcile", request.IdempotencyKey, err)
	}
	client, err := a.clients(ctx, target.Region, credential)
	if err != nil {
		return adapter.ReconcileResult{}, retryable("reconcile", request.IdempotencyKey, err)
	}
	current, err := describe(ctx, client, target)
	if err != nil {
		return adapter.ReconcileResult{}, retryable("describe-services", request.IdempotencyKey, err)
	}
	if current == desired {
		expectations := target.RollbackContainerDigests
		if request.Operation != adapter.ReconcileRollback {
			expectations = target.TaskDefinitions[request.ArtifactDigest].ExpectedContainerDigests
		}
		deployment, err := a.confirmRuntime(ctx, client, target, desired, expectations, request.IdempotencyKey)
		if err != nil {
			return adapter.ReconcileResult{Status: adapter.ReconcilePending}, err
		}
		return adapter.ReconcileResult{Status: adapter.ReconcileSucceeded, Deployment: deployment}, nil
	}
	if slices.Contains(pending, current) {
		return adapter.ReconcileResult{Status: adapter.ReconcilePending}, nil
	}
	return adapter.ReconcileResult{Status: adapter.ReconcileFailed}, nil
}

func (a *Adapter) reconcileTarget(request adapter.ReconcileRequest) (Target, string, []string, error) {
	switch request.Operation {
	case "", adapter.ReconcileDeploy:
		if err := adapter.ValidateDeployRequest(adapter.DeployRequest{Target: request.Target, ArtifactDigest: request.ArtifactDigest, IdempotencyKey: request.IdempotencyKey}); err != nil {
			return Target{}, "", nil, err
		}
		target, definition, err := a.resolve(request.Target, request.ArtifactDigest)
		return target, definition.ARN, definition.ExpectedTaskDefinitions, err
	case adapter.ReconcileRollback:
		if err := adapter.ValidateRollbackRequest(adapter.RollbackRequest{Target: request.Target, OriginalDeployment: request.OriginalDeployment, IdempotencyKey: request.IdempotencyKey}); err != nil {
			return Target{}, "", nil, err
		}
		target, ok := a.config.Targets[TargetKey{Service: request.Target.Service, Environment: request.Target.Environment}]
		if !ok || target.RollbackTaskDefinition == "" {
			return Target{}, "", nil, errors.New("trusted rollback task definition is unavailable")
		}
		// Match Rollback's trusted predecessor set. Neither the artifact digest
		// nor the original execution ID can choose a provider resource.
		var pending []string
		for _, definition := range target.TaskDefinitions {
			pending = append(pending, definition.ARN)
			pending = append(pending, definition.ExpectedTaskDefinitions...)
		}
		return target, target.RollbackTaskDefinition, pending, nil
	default:
		return Target{}, "", nil, errors.New("unsupported reconciliation operation")
	}
}
