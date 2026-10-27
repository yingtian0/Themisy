package protocol

import (
	"reflect"
	"time"

	"themisy/pkg/adapter"
)

type ResultStatus string

const (
	ResultSucceeded      ResultStatus = "SUCCEEDED"
	ResultRejected       ResultStatus = "REJECTED"
	ResultUnknown        ResultStatus = "UNKNOWN"
	ResultReconciliation ResultStatus = "RECONCILIATION_REQUIRED"
)

type Result struct {
	Health              *adapter.Evidence           `json:"health,omitempty"`
	Runtime             *adapter.RuntimeObservation `json:"runtime,omitempty"`
	ProtocolVersion     string                      `json:"protocol_version"`
	GrantID             string                      `json:"grant_id"`
	RunID               string                      `json:"run_id"`
	StepID              string                      `json:"step_id"`
	Status              ResultStatus                `json:"status"`
	ReasonCode          string                      `json:"reason_code,omitempty"`
	ExternalExecutionID string                      `json:"external_execution_id,omitempty"`
	CompletedAt         time.Time                   `json:"completed_at"`
	EvidenceHash        string                      `json:"evidence_hash,omitempty"`
	Evidence            *adapter.Evidence           `json:"evidence,omitempty"`
	Signature           Signature                   `json:"signature,omitempty"`
}

// ResultEqual compares durable protocol values, including optional evidence
// bodies that are represented by pointers for backward-compatible omission.
func ResultEqual(a, b Result) bool { return reflect.DeepEqual(a, b) }
