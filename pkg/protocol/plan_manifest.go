package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const PlanManifestVersion = "themisy.plan-manifest/v1alpha1"

// PlanManifest is independently provisioned, never reconstructed by a Runner
// from an ActionGrant. Revision is monotonic within a tenant/group/run.
type PlanManifest struct {
	ProtocolVersion  string    `json:"protocol_version"`
	Issuer           string    `json:"issuer"`
	TenantID         string    `json:"tenant_id"`
	RunnerGroup      string    `json:"runner_group"`
	RunID            string    `json:"run_id"`
	StepID           string    `json:"step_id"`
	Revision         int64     `json:"revision"`
	PlanHash         string    `json:"plan_hash"`
	ContractHash     string    `json:"contract_hash"`
	ProfileHash      string    `json:"profile_hash"`
	PolicyHash       string    `json:"policy_hash"`
	EvidenceHash     string    `json:"evidence_hash"`
	Target           Target    `json:"target"`
	Action           Action    `json:"action"`
	ApprovalRequired bool      `json:"approval_required"`
	ApprovalProofs   []string  `json:"approval_proofs"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Signature        Signature `json:"signature"`
}

func CanonicalManifestPayload(m PlanManifest) ([]byte, error) {
	if m.ProtocolVersion != PlanManifestVersion || m.Revision < 1 || m.Issuer == "" || m.TenantID == "" || m.RunnerGroup == "" || m.RunID == "" || m.StepID == "" || m.Target.Service == "" || m.Target.Environment == "" || m.IssuedAt.IsZero() || !m.ExpiresAt.After(m.IssuedAt) {
		return nil, errors.New("invalid plan manifest")
	}
	for _, d := range []string{m.PlanHash, m.ContractHash, m.ProfileHash, m.PolicyHash, m.EvidenceHash, m.Action.ArtifactDigest} {
		if !strings.HasPrefix(d, "sha256:") || len(d) != 71 {
			return nil, errors.New("invalid manifest hash")
		}
		if _, err := hex.DecodeString(d[7:]); err != nil {
			return nil, err
		}
	}
	if m.Action.Capability != CapabilityDeploy && m.Action.Capability != CapabilityRollback {
		return nil, errors.New("invalid manifest capability")
	}
	if m.Action.Capability == CapabilityRollback && m.Action.ExternalExecutionID == "" {
		return nil, errors.New("rollback manifest requires execution ID")
	}
	if m.Action.Capability == CapabilityDeploy && m.Action.ExternalExecutionID != "" {
		return nil, errors.New("deploy manifest cannot name execution ID")
	}
	if m.ApprovalRequired && len(m.ApprovalProofs) == 0 {
		return nil, errors.New("manifest requires approval binding")
	}
	m.Signature = Signature{}
	m.ApprovalProofs = append([]string(nil), m.ApprovalProofs...)
	sort.Strings(m.ApprovalProofs)
	for i, p := range m.ApprovalProofs {
		if p == "" || (i > 0 && p == m.ApprovalProofs[i-1]) {
			return nil, errors.New("invalid approval binding")
		}
	}
	m.IssuedAt = m.IssuedAt.UTC()
	m.ExpiresAt = m.ExpiresAt.UTC()
	return json.Marshal(m)
}

func ManifestHash(m PlanManifest) (string, error) {
	b, err := CanonicalManifestPayload(m)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}
