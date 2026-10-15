package adapter

import (
	"context"

	"themisy/pkg/credentials"
)

// StatusRequest identifies a logical target only. Provider identifiers remain
// in trusted adapter configuration.
type StatusRequest struct {
	Target Target `json:"target"`
}

// DeploymentStatus is a read-only snapshot. It does not imply that a release
// is healthy or successful; runtime verification owns that decision.
type DeploymentStatus struct {
	ServiceStatus  string                    `json:"service_status"`
	TaskDefinition string                    `json:"task_definition"`
	DesiredCount   int32                     `json:"desired_count"`
	RunningCount   int32                     `json:"running_count"`
	PendingCount   int32                     `json:"pending_count"`
	Deployments    []ServiceDeploymentStatus `json:"deployments"`
}

type ServiceDeploymentStatus struct {
	TaskDefinition string `json:"task_definition"`
	Status         string `json:"status"`
	RolloutState   string `json:"rollout_state,omitempty"`
	RunningCount   int32  `json:"running_count"`
	DesiredCount   int32  `json:"desired_count"`
}

// StatusReader is an optional typed, read-only capability for provider
// adapters. It never accepts arbitrary provider resource identifiers.
type StatusReader interface {
	Status(context.Context, StatusRequest, credentials.Credential) (DeploymentStatus, error)
}
