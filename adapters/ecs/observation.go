package ecs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"time"
)

const RuntimeObservationSchema = "themisy.ecs-runtime/v1alpha1"

var ErrRuntimeObservation = errors.New("ECS_RUNTIME_OBSERVATION_UNAVAILABLE")

type RuntimeSummary struct {
	DesiredCount int32  `json:"desired_count"`
	RunningCount int32  `json:"running_count"`
	PendingCount int32  `json:"pending_count"`
	TaskCount    int    `json:"task_count"`
	RolloutState string `json:"rollout_state"`
}

// RuntimeObservation is inspectable source data, not an execution authorization.
// Failed observations carry no invented partial runtime counts.
type RuntimeObservation struct {
	SchemaVersion    string                     `json:"schema_version"`
	Target           adapter.Target             `json:"target"`
	Operation        adapter.ReconcileOperation `json:"operation"`
	RequestHash      string                     `json:"request_hash"`
	DefinitionHash   string                     `json:"definition_hash"`
	ContainerSetHash string                     `json:"container_set_hash"`
	Status           adapter.VerificationStatus `json:"status"`
	ReasonCode       string                     `json:"reason_code"`
	Summary          RuntimeSummary             `json:"summary"`
	ObservedAt       time.Time                  `json:"observed_at"`
	AdapterVersion   string                     `json:"adapter_version"`
	Hash             string                     `json:"hash"`
}

func runtimeHash(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func RuntimeObservationHash(v RuntimeObservation) (string, error) { v.Hash = ""; return runtimeHash(v) }

// AuthorizeRuntime checks only the configured logical read selector, not a Grant.
func (a *Adapter) AuthorizeRuntime(r adapter.ReconcileRequest) error {
	if a == nil {
		return ErrRuntimeObservation
	}
	_, _, _, err := a.reconcileTarget(r)
	if err != nil {
		return ErrRuntimeObservation
	}
	return nil
}

// ReadRuntime uses DescribeTaskDefinition/DescribeServices/ListTasks/DescribeTasks
// only. It never dispatches, retries a write, or promotes API acceptance to PASS.
func (a *Adapter) ReadRuntime(ctx context.Context, r adapter.ReconcileRequest, credential credentials.Credential) (RuntimeObservation, error) {
	if a == nil || ctx.Err() != nil {
		return RuntimeObservation{}, ErrRuntimeObservation
	}
	target, definition, _, err := a.reconcileTarget(r)
	if err != nil {
		return RuntimeObservation{}, ErrRuntimeObservation
	}
	client, err := a.clients(ctx, target.Region, credential)
	if err != nil {
		return RuntimeObservation{}, ErrRuntimeObservation
	}
	containers := target.RollbackContainerDigests
	operation := r.Operation
	if operation == "" {
		operation = adapter.ReconcileDeploy
	}
	if operation == adapter.ReconcileDeploy {
		containers = target.TaskDefinitions[r.ArtifactDigest].ExpectedContainerDigests
	}
	started := a.now().UTC()
	if started.IsZero() {
		return RuntimeObservation{}, ErrRuntimeObservation
	}
	observation := RuntimeObservation{SchemaVersion: RuntimeObservationSchema, Target: r.Target, Operation: operation, Status: adapter.VerificationInconclusive, ReasonCode: "RUNTIME_NOT_CONFIRMED", AdapterVersion: Name + "/" + Version}
	// Hash only the normalized logical selector; raw provider resource identifiers
	// and errors do not enter exported observations.
	observation.RequestHash, err = runtimeHash(struct {
		Target                                              adapter.Target
		Operation                                           adapter.ReconcileOperation
		ArtifactDigest, IdempotencyKey, OriginalExecutionID string
	}{r.Target, operation, r.ArtifactDigest, r.IdempotencyKey, r.OriginalDeployment.ExternalExecutionID})
	if err != nil {
		return RuntimeObservation{}, ErrRuntimeObservation
	}
	observation.DefinitionHash, _ = runtimeHash(definition)
	observation.ContainerSetHash, _ = runtimeHash(containers)
	if err := verifyTaskDefinition(ctx, client, definition, containers, r.IdempotencyKey); err == nil {
		if summary, err := observeRuntimeSummary(ctx, client, target, definition, containers); err == nil {
			observation.Status = adapter.VerificationPass
			observation.ReasonCode = "RUNTIME_CONFIRMED"
			observation.Summary = summary
		}
	}
	observation.ObservedAt = a.now().UTC()
	if ctx.Err() != nil || observation.ObservedAt.Before(started) {
		return RuntimeObservation{}, ErrRuntimeObservation
	}
	observation.Hash, err = RuntimeObservationHash(observation)
	if err != nil {
		return RuntimeObservation{}, ErrRuntimeObservation
	}
	return observation, nil
}
