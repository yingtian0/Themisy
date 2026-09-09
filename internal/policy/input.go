package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"themisy/internal/domain"
	"themisy/pkg/protocol"
)

func PreDispatchEvidenceHash() string {
	sum := sha256.Sum256([]byte("themisy/pre-dispatch-evidence/v1"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

const InputVersionV1Alpha1 = "themisy.policy.input/v1alpha1"

// Input is the sole canonical policy input used by both Control Plane and Runner.
// Trusted fields are populated only after identity and delegation verification.
type Input struct {
	Version              string              `json:"version,omitempty"`
	TenantID             string              `json:"tenant_id,omitempty"`
	RunnerGroup          string              `json:"runner_group,omitempty"`
	SubjectType          string              `json:"subject_type,omitempty"`
	UserID               string              `json:"user_id"`
	UserIssuer           string              `json:"user_issuer,omitempty"`
	AgentID              string              `json:"agent_id"`
	DelegationRef        string              `json:"delegation_ref,omitempty"`
	AgentScopes          []string            `json:"-"` // Packet 00-02 compatibility only; never trusted or signed.
	Environment          domain.Environment  `json:"environment"`
	Service              string              `json:"service"`
	Capability           protocol.Capability `json:"capability,omitempty"`
	ArtifactDigest       string              `json:"artifact_digest,omitempty"`
	Risk                 string              `json:"risk"`
	PlanHash             string              `json:"plan_hash,omitempty"`
	ContractHash         string              `json:"contract_hash,omitempty"`
	ProfileHash          string              `json:"profile_hash,omitempty"`
	PolicyHash           string              `json:"policy_hash,omitempty"`
	EvidenceHash         string              `json:"evidence_hash,omitempty"`
	ApprovalProofs       []string            `json:"approval_proofs,omitempty"`
	CISuccess            bool                `json:"ci_success"`
	DependenciesHealthy  bool                `json:"dependencies_healthy"`
	DestructiveMigration bool                `json:"-"` // represented by risk, approval, and evidence in v1alpha1.
}

func NormalizeInput(input Input) Input {
	if input.Version == "" {
		input.Version = InputVersionV1Alpha1
	}
	input.AgentScopes = append([]string(nil), input.AgentScopes...)
	input.ApprovalProofs = append([]string(nil), input.ApprovalProofs...)
	sort.Strings(input.ApprovalProofs)
	return input
}

func CanonicalInput(input Input) ([]byte, error) { return json.Marshal(NormalizeInput(input)) }

func InputHash(input Input) (string, error) {
	canonical, err := CanonicalInput(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func InputForRelease(run domain.ReleaseRun, step domain.ReleaseStep) Input {
	result := Input{Version: InputVersionV1Alpha1, TenantID: run.TenantID, RunnerGroup: run.RunnerGroup, UserID: run.RequestedBy, UserIssuer: run.SubjectIssuer, AgentID: run.Agent.ID, DelegationRef: run.DelegationRef, AgentScopes: append([]string(nil), run.Agent.Scopes...), Environment: run.Environment, Service: step.Service, Capability: protocol.CapabilityDeploy, ArtifactDigest: step.Change.DesiredVersion, Risk: step.Change.Risk, PlanHash: run.Plan.Hash, PolicyHash: run.Plan.PolicyHash, EvidenceHash: run.Plan.EvidenceHash, CISuccess: step.Change.CISuccess, DependenciesHealthy: step.Change.DependenciesHealthy, DestructiveMigration: step.Change.DestructiveMigration}
	result.SubjectType = run.SubjectType
	for _, phase := range run.Plan.Phases {
		for _, planned := range phase.Steps {
			if planned.Service == step.Service {
				result.ContractHash, result.ProfileHash = planned.ContractHash, planned.ProfileHash
				if planned.Scheduling.RunnerGroup != "" {
					result.RunnerGroup = planned.Scheduling.RunnerGroup
				}
				break
			}
		}
	}
	if step.Approval != nil && step.Approval.Status == domain.ApprovalApproved {
		result.ApprovalProofs = []string{step.Approval.ID}
	}
	return NormalizeInput(result)
}

func InputForGrant(grant protocol.ActionGrant, userIssuer, verifiedUser, verifiedAgent, verifiedDelegation string) Input {
	input := Input{Version: InputVersionV1Alpha1, TenantID: grant.TenantID, RunnerGroup: grant.RunnerGroup, UserID: verifiedUser, UserIssuer: userIssuer, AgentID: verifiedAgent, DelegationRef: verifiedDelegation, Environment: domain.Environment(grant.Target.Environment), Service: grant.Target.Service, Capability: grant.Action.Capability, ArtifactDigest: grant.Action.ArtifactDigest, Risk: grant.Risk, PlanHash: grant.PlanHash, ContractHash: grant.ContractHash, ProfileHash: grant.ProfileHash, PolicyHash: grant.PolicyHash, EvidenceHash: grant.EvidenceHash, ApprovalProofs: append([]string(nil), grant.ApprovalProofs...), CISuccess: true, DependenciesHealthy: true}
	input.SubjectType = grant.SubjectType
	return NormalizeInput(input)
}
