package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"themisy/pkg/credentials"
)

type DeployRequest struct {
	Target         Target `json:"target"`
	ArtifactDigest string `json:"artifact_digest"`
	IdempotencyKey string `json:"idempotency_key"`
}

type RollbackRequest struct {
	Target             Target     `json:"target"`
	OriginalDeployment Deployment `json:"original_deployment"`
	IdempotencyKey     string     `json:"idempotency_key"`
}

type ReconcileRequest struct {
	IdempotencyKey string    `json:"idempotency_key"`
	DispatchedAt   time.Time `json:"dispatched_at"`
	Target         Target    `json:"target,omitempty"`
	ArtifactDigest string    `json:"artifact_digest,omitempty"`
}

type ReconcileResult struct {
	Status     ReconcileStatus `json:"status"`
	Deployment Deployment      `json:"deployment"`
}

type DeployAdapter interface {
	Name() string
	Version() string
	Deploy(context.Context, DeployRequest, credentials.Credential) (Deployment, error)
	Rollback(context.Context, RollbackRequest, credentials.Credential) (Deployment, error)
	Reconcile(context.Context, ReconcileRequest, credentials.Credential) (ReconcileResult, error)
}

func ValidateDeployRequest(request DeployRequest) error {
	if strings.TrimSpace(request.Target.Service) == "" || strings.TrimSpace(request.Target.Environment) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return errors.New("target service, environment, and idempotency key are required")
	}
	return validateDigest(request.ArtifactDigest)
}

func ValidateRollbackRequest(request RollbackRequest) error {
	if strings.TrimSpace(request.Target.Service) == "" || strings.TrimSpace(request.Target.Environment) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.OriginalDeployment.ExternalExecutionID) == "" {
		return errors.New("target, original external execution ID, and idempotency key are required")
	}
	return nil
}

func CorrelationID(idempotencyKey string) string {
	sum := sha256.Sum256([]byte(idempotencyKey))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateDigest(value string) error {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return errors.New("artifact digest must be sha256")
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	if err != nil {
		return errors.New("artifact digest must be sha256")
	}
	return nil
}
