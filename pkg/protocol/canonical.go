package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

func CanonicalGrantPayload(grant ActionGrant) ([]byte, error) {
	copy := grant.SigningCopy()
	if err := validateGrantShape(copy); err != nil {
		return nil, err
	}
	return json.Marshal(copy)
}

func GrantHash(grant ActionGrant) (string, error) {
	payload, err := CanonicalGrantPayload(grant)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func DecodeActionGrant(reader io.Reader) (ActionGrant, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var grant ActionGrant
	if err := decoder.Decode(&grant); err != nil {
		return ActionGrant{}, fmt.Errorf("decode action grant: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ActionGrant{}, fmt.Errorf("decode action grant: trailing JSON")
	}
	if _, err := CanonicalGrantPayload(grant); err != nil {
		return ActionGrant{}, err
	}
	return grant, nil
}

func EncodeActionGrant(grant ActionGrant) ([]byte, error) {
	if _, err := CanonicalGrantPayload(grant); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(grant); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buffer.Bytes()), nil
}

func validateGrantShape(grant ActionGrant) error {
	if grant.PolicyRevision < 0 {
		return fmt.Errorf("invalid Policy revision")
	}
	if err := ValidateVersion(grant.ProtocolVersion); err != nil {
		return err
	}
	required := map[string]string{
		"grant_id": grant.GrantID, "issuer": grant.Issuer, "tenant_id": grant.TenantID,
		"runner_group": grant.RunnerGroup, "run_id": grant.RunID, "step_id": grant.StepID,
		"subject_type": grant.SubjectType,
		"user_subject": grant.UserSubject, "user_identity_proof": grant.UserIdentityProof,
		"agent_id": grant.AgentID, "delegation_ref": grant.DelegationRef,
		"target.service": grant.Target.Service, "target.environment": grant.Target.Environment,
		"action.capability": string(grant.Action.Capability), "action.artifact_digest": grant.Action.ArtifactDigest,
		"risk": grant.Risk, "plan_hash": grant.PlanHash, "contract_hash": grant.ContractHash,
		"profile_hash": grant.ProfileHash, "policy_hash": grant.PolicyHash,
		"policy_input_hash": grant.PolicyInputHash, "evidence_hash": grant.EvidenceHash,
		"idempotency_key": grant.IdempotencyKey, "nonce": grant.Nonce,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("INVALID_ACTION_GRANT: %s is required", field)
		}
	}
	if grant.SubjectType != "user" && grant.SubjectType != "ci" {
		return fmt.Errorf("INVALID_ACTION_GRANT: subject_type must be user or ci")
	}
	if grant.IssuedAt.IsZero() || grant.ExpiresAt.IsZero() || !grant.ExpiresAt.After(grant.IssuedAt) {
		return fmt.Errorf("INVALID_ACTION_GRANT: valid issued_at and expires_at are required")
	}
	for _, field := range []struct{ name, value string }{
		{"artifact_digest", grant.Action.ArtifactDigest}, {"plan_hash", grant.PlanHash},
		{"contract_hash", grant.ContractHash}, {"profile_hash", grant.ProfileHash},
		{"policy_hash", grant.PolicyHash}, {"policy_input_hash", grant.PolicyInputHash},
		{"evidence_hash", grant.EvidenceHash},
	} {
		if !strings.HasPrefix(field.value, "sha256:") || len(field.value) != len("sha256:")+64 {
			return fmt.Errorf("INVALID_ACTION_GRANT: %s must be a sha256 digest", field.name)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(field.value, "sha256:")); err != nil {
			return fmt.Errorf("INVALID_ACTION_GRANT: %s must be a sha256 digest", field.name)
		}
	}
	if grant.Action.Capability != CapabilityVerify && (grant.Action.SourceExternalExecutionID != "" || grant.Action.SourceNonce != "" || grant.Action.SourceRequestHash != "" || grant.Action.SourceIdempotencyKey != "" || grant.Action.SourceCapability != "" || grant.Action.SourceGrantID != "" || grant.Action.VerificationFrom != "" || grant.Action.VerificationTo != "") {
		return fmt.Errorf("INVALID_ACTION_GRANT: write action cannot contain verification selectors")
	}
	switch grant.Action.Capability {
	case CapabilityVerify:
		from, first := time.Parse(time.RFC3339Nano, grant.Action.VerificationFrom)
		to, second := time.Parse(time.RFC3339Nano, grant.Action.VerificationTo)
		if first != nil || second != nil || from.IsZero() || !to.After(from) || to.Sub(from) > 24*time.Hour || to.After(grant.IssuedAt) || grant.Action.SourceGrantID == "" || grant.Action.SourceNonce == "" || grant.Action.SourceRequestHash == "" || grant.Action.SourceIdempotencyKey == "" || (grant.Action.SourceCapability != CapabilityDeploy && grant.Action.SourceCapability != CapabilityRollback) || grant.Action.ExternalExecutionID == "" {
			return fmt.Errorf("INVALID_ACTION_GRANT: invalid source-bound verification request")
		}
	case CapabilityDeploy:
		if grant.Action.ExternalExecutionID != "" {
			return fmt.Errorf("INVALID_ACTION_GRANT: deploy cannot name an external execution")
		}
	case CapabilityRollback:
		if strings.TrimSpace(grant.Action.ExternalExecutionID) == "" {
			return fmt.Errorf("INVALID_ACTION_GRANT: rollback requires external_execution_id")
		}
	default:
		return fmt.Errorf("INVALID_ACTION_GRANT: unsupported capability %q", grant.Action.Capability)
	}
	return nil
}
