// Package adapter is the stable public SDK for typed Runner adapters. It does
// not depend on internal application or domain packages.
package adapter

import "time"

const (
	ProviderGitHubActions = "github-actions"
	ProviderDatadog       = "datadog"
	ProviderAWS           = "aws"
)

type Target struct {
	Service     string `json:"service"`
	Environment string `json:"environment"`
}

type Deployment struct {
	ExternalExecutionID string    `json:"external_execution_id"`
	CorrelationID       string    `json:"correlation_id"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at,omitempty"`
}

type ReconcileStatus string

const (
	ReconcileNotFound  ReconcileStatus = "NOT_FOUND"
	ReconcilePending   ReconcileStatus = "PENDING"
	ReconcileSucceeded ReconcileStatus = "SUCCEEDED"
	ReconcileFailed    ReconcileStatus = "FAILED"
)
