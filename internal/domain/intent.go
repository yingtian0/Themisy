package domain

const (
	ReleaseIntentAPIVersion = "execution.themisy.io/v1alpha1"
	ReleaseIntentKind       = "ReleaseIntent"
)

type ReleaseIntent struct {
	APIVersion        string        `json:"api_version"`
	Kind              string        `json:"kind"`
	RequestID         string        `json:"request_id"`
	ReleaseVersion    string        `json:"release_version"`
	TenantID          string        `json:"tenant_id,omitempty"`
	Environment       Environment   `json:"environment"`
	Region            string        `json:"region,omitempty"`
	Cluster           string        `json:"cluster,omitempty"`
	RequestedBy       string        `json:"requested_by"`
	SubjectType       string        `json:"subject_type,omitempty"`
	SubjectIssuer     string        `json:"subject_issuer,omitempty"`
	UserIdentityProof string        `json:"user_identity_proof,omitempty"`
	DelegationRef     string        `json:"delegation_ref,omitempty"`
	Agent             AgentIdentity `json:"delegated_agent"`
	Changes           []Change      `json:"changes"`
}
