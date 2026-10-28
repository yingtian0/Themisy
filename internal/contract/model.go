package contract

import "themisy/internal/domain"

const (
	APIVersion = "execution.themisy.io/v1alpha1"
	Kind       = "ServiceContract"
)

type ServiceContract struct {
	APIVersion   string                         `json:"apiVersion" yaml:"apiVersion"`
	Kind         string                         `json:"kind" yaml:"kind"`
	Metadata     Metadata                       `json:"metadata" yaml:"metadata"`
	Environments map[string]EnvironmentContract `json:"environments" yaml:"environments"`
	Dependencies []domain.Dependency            `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Verification Verification                   `json:"verification" yaml:"verification"`
	Rollback     Rollback                       `json:"rollback" yaml:"rollback"`
	ContentHash  string                         `json:"-" yaml:"-"`
	Source       string                         `json:"-" yaml:"-"`
}

type Metadata struct {
	Name       string `json:"name" yaml:"name"`
	Owner      string `json:"owner" yaml:"owner"`
	Repository string `json:"repository" yaml:"repository"`
	RiskTier   string `json:"riskTier" yaml:"riskTier"`
}

type EnvironmentContract struct {
	RollbackTargetHash string              `json:"rollbackTargetHash,omitempty" yaml:"rollbackTargetHash,omitempty"`
	ReleaseProfile     string              `json:"releaseProfile" yaml:"releaseProfile"`
	RunnerGroup        string              `json:"runnerGroup" yaml:"runnerGroup"`
	Capabilities       []domain.Capability `json:"capabilities" yaml:"capabilities"`
}

type Verification struct {
	Profile           string              `json:"profile" yaml:"profile"`
	ObservationWindow string              `json:"observationWindow" yaml:"observationWindow"`
	Checks            []VerificationCheck `json:"checks" yaml:"checks"`
}

type VerificationCheck struct {
	Type      string `json:"type" yaml:"type"`
	Threshold string `json:"threshold,omitempty" yaml:"threshold,omitempty"`
	Check     string `json:"check,omitempty" yaml:"check,omitempty"`
}

type Rollback struct {
	Capability string `json:"capability" yaml:"capability"`
	Strategy   string `json:"strategy" yaml:"strategy"`
	Deadline   string `json:"deadline" yaml:"deadline"`
}
