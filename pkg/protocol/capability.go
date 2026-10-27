package protocol

type Capability string

const (
	CapabilityVerify   Capability = "release.verify"
	CapabilityDeploy   Capability = "release.deploy"
	CapabilityRollback Capability = "release.rollback"
)

type Action struct {
	SourceExternalExecutionID string     `json:"source_external_execution_id,omitempty"`
	SourceNonce               string     `json:"source_nonce,omitempty"`
	SourceRequestHash         string     `json:"source_request_hash,omitempty"`
	SourceIdempotencyKey      string     `json:"source_idempotency_key,omitempty"`
	SourceCapability          Capability `json:"source_capability,omitempty"`
	SourceGrantID             string     `json:"source_grant_id,omitempty"`
	VerificationFrom          string     `json:"verification_from,omitempty"`
	VerificationTo            string     `json:"verification_to,omitempty"`
	Capability                Capability `json:"capability"`
	ArtifactDigest            string     `json:"artifact_digest"`
	ExternalExecutionID       string     `json:"external_execution_id,omitempty"`
}

type Target struct {
	Service     string `json:"service"`
	Environment string `json:"environment"`
}

type Capabilities struct {
	ProtocolVersions []string     `json:"protocol_versions"`
	Actions          []Capability `json:"actions"`
	RunnerGroup      string       `json:"runner_group"`
}
