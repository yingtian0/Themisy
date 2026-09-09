package domain

import "time"

type Environment string

const (
	EnvironmentStaging    Environment = "staging"
	EnvironmentProduction Environment = "production"
)

type Service struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	OwnerTeam       string                 `json:"owner_team"`
	RiskTier        string                 `json:"risk_tier,omitempty"`
	Repository      string                 `json:"repository"`
	ReleasePhase    int                    `json:"release_phase"`
	Dependencies    []string               `json:"dependencies"`
	RunnerGroups    map[Environment]string `json:"runner_groups,omitempty"`
	FailureDomains  []string               `json:"failure_domains,omitempty"`
	MetadataVersion string                 `json:"metadata_version"`
	UpdatedAt       time.Time              `json:"updated_at"`
}

type AgentIdentity struct {
	ID     string   `json:"id"`
	Scopes []string `json:"scopes"`
}

type Change struct {
	Service              string `json:"service"`
	DesiredVersion       string `json:"desired_version"`
	CISuccess            bool   `json:"ci_success"`
	DependenciesHealthy  bool   `json:"dependencies_healthy"`
	DestructiveMigration bool   `json:"destructive_migration,omitempty"`
	Risk                 string `json:"risk,omitempty"`
}

type ReleaseRequest struct {
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

type PlanStep struct {
	Service              string            `json:"service"`
	DesiredVersion       string            `json:"desired_version"`
	Phase                int               `json:"phase"`
	Profile              string            `json:"profile,omitempty"`
	Dependencies         []Dependency      `json:"dependencies,omitempty"`
	RequiredCapabilities []Capability      `json:"required_capabilities,omitempty"`
	ContractHash         string            `json:"contract_hash,omitempty"`
	ProfileHash          string            `json:"profile_hash,omitempty"`
	ChangeHash           string            `json:"change_hash,omitempty"`
	VerificationRequired bool              `json:"verification_required"`
	ObservationWindow    string            `json:"observation_window,omitempty"`
	RollbackMode         RollbackMode      `json:"rollback_mode"`
	Scheduling           SchedulingContext `json:"scheduling"`
}

type SchedulingContext struct {
	TenantID       string      `json:"tenant_id"`
	Environment    Environment `json:"environment"`
	Region         string      `json:"region"`
	Cluster        string      `json:"cluster"`
	Team           string      `json:"team"`
	RiskTier       string      `json:"risk_tier"`
	RunnerGroup    string      `json:"runner_group"`
	FailureDomains []string    `json:"failure_domains,omitempty"`
}

type PlanPhase struct {
	Number int        `json:"number"`
	Steps  []PlanStep `json:"steps"`
}

type ReleasePlan struct {
	APIVersion         string            `json:"api_version,omitempty"`
	ID                 string            `json:"plan_id,omitempty"`
	ReleaseVersion     string            `json:"release_version"`
	Environment        Environment       `json:"environment"`
	Phases             []PlanPhase       `json:"phases"`
	Hash               string            `json:"hash"`
	PlanHash           string            `json:"plan_hash,omitempty"`
	ContractHashes     map[string]string `json:"contract_hashes,omitempty"`
	ProfileHashes      map[string]string `json:"profile_hashes,omitempty"`
	ContextSnapshotRef string            `json:"context_snapshot_ref,omitempty"`
	PolicyHash         string            `json:"policy_hash,omitempty"`
	EvidenceHash       string            `json:"evidence_hash,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	ExpiresAt          time.Time         `json:"expires_at,omitempty"`
}

type RunStatus string

const (
	RunPending         RunStatus = "PENDING"
	RunRunning         RunStatus = "RUNNING"
	RunPaused          RunStatus = "PAUSED"
	RunWaitingApproval RunStatus = "WAITING_APPROVAL"
	RunSucceeded       RunStatus = "SUCCEEDED"
	RunBlocked         RunStatus = "BLOCKED"
	RunFailed          RunStatus = "FAILED"
	RunEscalated       RunStatus = "ESCALATED"
	RunCancelled       RunStatus = "CANCELLED"
)

type StepStatus string

const (
	StepPending         StepStatus = "PENDING"
	StepWaitingApproval StepStatus = "WAITING_APPROVAL"
	StepExecuting       StepStatus = "EXECUTING"
	StepVerifying       StepStatus = "VERIFYING"
	StepSucceeded       StepStatus = "SUCCEEDED"
	StepBlocked         StepStatus = "BLOCKED"
	StepRollingBack     StepStatus = "ROLLING_BACK"
	StepRolledBack      StepStatus = "ROLLED_BACK"
	StepEscalated       StepStatus = "ESCALATED"
	StepUnknown         StepStatus = "UNKNOWN"
	StepCancelled       StepStatus = "CANCELLED"
)

type Decision string

const (
	DecisionAllow           Decision = "ALLOW"
	DecisionDeny            Decision = "DENY"
	DecisionRequireApproval Decision = "REQUIRE_APPROVAL"
)

type PolicyDecision struct {
	Decision      Decision  `json:"decision"`
	PolicyVersion string    `json:"policy_version"`
	ReasonCode    string    `json:"reason_code"`
	ReasonDetail  string    `json:"reason_detail"`
	RequiredRoles []string  `json:"required_roles,omitempty"`
	InputHash     string    `json:"input_hash"`
	CreatedAt     time.Time `json:"created_at"`
}

type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "PENDING"
	ApprovalApproved ApprovalStatus = "APPROVED"
	ApprovalDenied   ApprovalStatus = "DENIED"
	ApprovalExpired  ApprovalStatus = "EXPIRED"
	ApprovalRevoked  ApprovalStatus = "REVOKED"
)

type Approval struct {
	ID            string         `json:"id"`
	RequiredRoles []string       `json:"required_roles"`
	Status        ApprovalStatus `json:"status"`
	PlanHash      string         `json:"plan_hash"`
	RequestedAt   time.Time      `json:"requested_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
	DecidedBy     string         `json:"decided_by,omitempty"`
	DecidedAt     *time.Time     `json:"decided_at,omitempty"`
}

type ApprovalSummary struct {
	Approval Approval `json:"approval"`
	RunID    string   `json:"run_id"`
	Service  string   `json:"service"`
	TenantID string   `json:"tenant_id"`
}

type RunnerStatus string

const (
	RunnerUnknown  RunnerStatus = "UNKNOWN"
	RunnerReady    RunnerStatus = "READY"
	RunnerDraining RunnerStatus = "DRAINING"
	RunnerFrozen   RunnerStatus = "FROZEN"
)

type RunnerInfo struct {
	ID               string       `json:"id"`
	TenantID         string       `json:"tenant_id"`
	Group            string       `json:"group"`
	Status           RunnerStatus `json:"status"`
	Capacity         int          `json:"capacity"`
	ReportedCapacity int          `json:"reported_capacity,omitempty"`
	InFlight         int          `json:"in_flight,omitempty"`
	LastSeen         time.Time    `json:"last_seen,omitempty"`
	StateVersion     int64        `json:"state_version,omitempty"`
	ControlledBy     string       `json:"controlled_by,omitempty"`
	ControlledAt     *time.Time   `json:"controlled_at,omitempty"`
	FrozenBy         string       `json:"frozen_by,omitempty"`
	FrozenAt         *time.Time   `json:"frozen_at,omitempty"`
}

type ContractValidation struct {
	Valid       bool   `json:"valid"`
	Name        string `json:"name,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

type Execution struct {
	ID                  string    `json:"id"`
	Adapter             string    `json:"adapter,omitempty"`
	IdempotencyKey      string    `json:"idempotency_key"`
	Status              string    `json:"status,omitempty"`
	ExternalExecutionID string    `json:"external_execution_id"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
}

type VerificationStatus string

const (
	VerificationPass         VerificationStatus = "PASS"
	VerificationFail         VerificationStatus = "FAIL"
	VerificationInconclusive VerificationStatus = "INCONCLUSIVE"
	VerificationMissing      VerificationStatus = "MISSING"
)

type Evidence struct {
	Source         string    `json:"source"`
	QueryHash      string    `json:"query_hash"`
	WindowFrom     time.Time `json:"window_from"`
	WindowTo       time.Time `json:"window_to"`
	ObservedAt     time.Time `json:"observed_at"`
	ObservedValue  float64   `json:"observed_value,omitempty"`
	Threshold      float64   `json:"threshold,omitempty"`
	AdapterVersion string    `json:"adapter_version"`
	EvidenceHash   string    `json:"evidence_hash"`
}

type Verification struct {
	Status        VerificationStatus `json:"status"`
	Healthy       bool               `json:"healthy"` // compatibility projection; Status is authoritative.
	Reason        string             `json:"reason"`
	ObservedValue float64            `json:"observed_value,omitempty"`
	Threshold     float64            `json:"threshold,omitempty"`
	CheckedAt     time.Time          `json:"checked_at"`
	Evidence      Evidence           `json:"evidence"`
}

type RollbackMode string

const (
	RollbackAutomatic        RollbackMode = "automatic"
	RollbackApprovalRequired RollbackMode = "approval-required"
	RollbackManualOnly       RollbackMode = "manual-only"
	RollbackUnsupported      RollbackMode = "unsupported"
)

type ReleaseStep struct {
	Service              string          `json:"service"`
	Phase                int             `json:"phase"`
	Wave                 int             `json:"wave"`
	Status               StepStatus      `json:"status"`
	Change               Change          `json:"change"`
	Policy               *PolicyDecision `json:"policy,omitempty"`
	Approval             *Approval       `json:"approval,omitempty"`
	Execution            *Execution      `json:"execution,omitempty"`
	Verification         *Verification   `json:"verification,omitempty"`
	VerificationRequired bool            `json:"verification_required"`
	ObservationWindow    string          `json:"observation_window,omitempty"`
	RollbackMode         RollbackMode    `json:"rollback_mode"`
	RollbackExecution    *Execution      `json:"rollback_execution,omitempty"`
	RollbackVerification *Verification   `json:"rollback_verification,omitempty"`
	Failure              string          `json:"failure,omitempty"`
}

type ReleaseRun struct {
	ID                string        `json:"id"`
	RequestID         string        `json:"request_id"`
	ReleaseVersion    string        `json:"release_version"`
	Environment       Environment   `json:"environment"`
	RequestedBy       string        `json:"requested_by"`
	SubjectType       string        `json:"subject_type,omitempty"`
	SubjectIssuer     string        `json:"subject_issuer,omitempty"`
	UserIdentityProof string        `json:"user_identity_proof,omitempty"`
	TenantID          string        `json:"tenant_id,omitempty"`
	Region            string        `json:"region,omitempty"`
	Cluster           string        `json:"cluster,omitempty"`
	RunnerGroup       string        `json:"runner_group,omitempty"`
	DelegationRef     string        `json:"delegation_ref,omitempty"`
	Agent             AgentIdentity `json:"delegated_agent"`
	Plan              ReleasePlan   `json:"plan"`
	Status            RunStatus     `json:"status"`
	PausedFrom        RunStatus     `json:"paused_from,omitempty"`
	Steps             []ReleaseStep `json:"steps"`
	StateVersion      int64         `json:"state_version"`
	WorkflowID        string        `json:"workflow_id,omitempty"`
	TemporalRunID     string        `json:"temporal_run_id,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

type OutboxEvent struct {
	ID            string         `json:"id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	AvailableAt   time.Time      `json:"available_at"`
	PublishedAt   *time.Time     `json:"published_at,omitempty"`
	Attempts      int            `json:"attempts"`
}

type AuditEvent struct {
	ID            string         `json:"id"`
	CorrelationID string         `json:"correlation_id"`
	ActorType     string         `json:"actor_type"`
	ActorID       string         `json:"actor_id"`
	DelegatedBy   string         `json:"delegated_by,omitempty"`
	Action        string         `json:"action"`
	ResourceType  string         `json:"resource_type"`
	ResourceID    string         `json:"resource_id"`
	Result        string         `json:"result"`
	Details       map[string]any `json:"details,omitempty"`
	Timestamp     time.Time      `json:"timestamp"`
}
