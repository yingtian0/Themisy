package protocol

import "time"

type ResultStatus string

const (
	ResultSucceeded      ResultStatus = "SUCCEEDED"
	ResultRejected       ResultStatus = "REJECTED"
	ResultUnknown        ResultStatus = "UNKNOWN"
	ResultReconciliation ResultStatus = "RECONCILIATION_REQUIRED"
)

type Result struct {
	ProtocolVersion     string       `json:"protocol_version"`
	GrantID             string       `json:"grant_id"`
	RunID               string       `json:"run_id"`
	StepID              string       `json:"step_id"`
	Status              ResultStatus `json:"status"`
	ReasonCode          string       `json:"reason_code,omitempty"`
	ExternalExecutionID string       `json:"external_execution_id,omitempty"`
	CompletedAt         time.Time    `json:"completed_at"`
	EvidenceHash        string       `json:"evidence_hash,omitempty"`
	Signature           Signature    `json:"signature,omitempty"`
}
