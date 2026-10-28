package protocol

import (
	"sort"
	"time"
)

type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value"`
}

type ActionGrant struct {
	PolicyRevision    int64     `json:"policy_revision,omitempty"`
	ProtocolVersion   string    `json:"protocol_version"`
	GrantID           string    `json:"grant_id"`
	Issuer            string    `json:"issuer"`
	TenantID          string    `json:"tenant_id"`
	RunnerGroup       string    `json:"runner_group"`
	RunID             string    `json:"run_id"`
	StepID            string    `json:"step_id"`
	SubjectType       string    `json:"subject_type"`
	UserSubject       string    `json:"user_subject"`
	UserIdentityProof string    `json:"user_identity_proof"`
	AgentID           string    `json:"agent_id"`
	DelegationRef     string    `json:"delegation_ref"`
	Target            Target    `json:"target"`
	Action            Action    `json:"action"`
	Risk              string    `json:"risk"`
	PlanHash          string    `json:"plan_hash"`
	ContractHash      string    `json:"contract_hash"`
	ProfileHash       string    `json:"profile_hash"`
	PolicyHash        string    `json:"policy_hash"`
	PolicyInputHash   string    `json:"policy_input_hash"`
	EvidenceHash      string    `json:"evidence_hash"`
	ApprovalProofs    []string  `json:"approval_proofs"`
	IdempotencyKey    string    `json:"idempotency_key"`
	Nonce             string    `json:"nonce"`
	IssuedAt          time.Time `json:"issued_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	Signature         Signature `json:"signature"`
}

func (g ActionGrant) SigningCopy() ActionGrant {
	g.Signature = Signature{}
	g.ApprovalProofs = append([]string(nil), g.ApprovalProofs...)
	sort.Strings(g.ApprovalProofs)
	return g
}
