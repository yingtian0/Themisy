package ecs

import (
	"context"
	"errors"
	"slices"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

func (a *Adapter) Reconcile(ctx context.Context, request adapter.ReconcileRequest, credential credentials.Credential) (adapter.ReconcileResult, error) {
	if request.IdempotencyKey == "" || request.Target.Service == "" || request.Target.Environment == "" || request.ArtifactDigest == "" {
		return adapter.ReconcileResult{}, terminal("reconcile", request.IdempotencyKey, errors.New("idempotency key, logical target, and artifact digest are required"))
	}
	target, definition, err := a.resolve(request.Target, request.ArtifactDigest)
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
	if current == definition.ARN {
		return adapter.ReconcileResult{Status: adapter.ReconcileSucceeded, Deployment: a.deployment(request.IdempotencyKey)}, nil
	}
	if slices.Contains(definition.ExpectedTaskDefinitions, current) {
		return adapter.ReconcileResult{Status: adapter.ReconcilePending}, nil
	}
	return adapter.ReconcileResult{Status: adapter.ReconcileFailed}, nil
}
