package contextsnapshot

import (
	"fmt"
	"strings"
	"themisy/adapters/githubactions"
	"themisy/internal/grant"
	"time"
)

type AdmissionConfig struct {
	Mode                 string                 `yaml:"mode"`
	File                 string                 `yaml:"file"`
	Issuer               string                 `yaml:"issuer"`
	TrustBundleFile      string                 `yaml:"trust_bundle_file"`
	BuildIssuer          string                 `yaml:"build_issuer"`
	BuildTrustBundleFile string                 `yaml:"build_trust_bundle_file"`
	MinimumRevision      int64                  `yaml:"minimum_revision"`
	MaximumAge           string                 `yaml:"maximum_age"`
	DependencyMaximumAge string                 `yaml:"dependency_maximum_age"`
	Targets              []CISourceConfig       `yaml:"targets"`
	Metadata             []MetadataSourceConfig `yaml:"metadata"`
}
type CISourceConfig struct {
	Service            string `yaml:"service"`
	Environment        string `yaml:"environment"`
	Owner              string `yaml:"owner"`
	Repository         string `yaml:"repository"`
	Workflow           string `yaml:"workflow"`
	MaximumAge         string `yaml:"maximum_age"`
	BuildStatementFile string `yaml:"build_statement_file"`
}
type MetadataSourceConfig struct {
	Service      string `yaml:"service"`
	Environment  string `yaml:"environment"`
	Owner        string `yaml:"owner"`
	Repository   string `yaml:"repository"`
	Revision     string `yaml:"revision"`
	ContractPath string `yaml:"contract_path"`
	ProfilePath  string `yaml:"profile_path"`
	MaximumAge   string `yaml:"maximum_age"`
}

func (c AdmissionConfig) Validate() error {
	if c.Mode == "synthetic" {
		return nil
	}
	if c.Mode != "signed" || c.File == "" || c.Issuer == "" || c.BuildIssuer == "" || c.Issuer == c.BuildIssuer || c.TrustBundleFile == "" || c.BuildTrustBundleFile == "" || c.MinimumRevision < 1 || len(c.Targets) == 0 || len(c.Targets) > 128 || len(c.Metadata) == 0 || len(c.Metadata) > 128 {
		return fmt.Errorf("signed Context configuration incomplete")
	}
	age, err := time.ParseDuration(c.MaximumAge)
	if err != nil || age <= 0 || age > 15*time.Minute {
		return fmt.Errorf("Context maximum_age must be within fifteen minutes")
	}
	depAge, err := time.ParseDuration(c.DependencyMaximumAge)
	if err != nil || depAge <= 0 || depAge > 24*time.Hour {
		return fmt.Errorf("dependency maximum_age invalid")
	}
	ci := githubactions.CIConfig{Targets: map[githubactions.CITargetKey]githubactions.CITarget{}}
	seen := map[string]bool{}
	for _, target := range c.Targets {
		key := target.Service + "\x00" + target.Environment
		if seen[key] {
			return fmt.Errorf("duplicate Context target")
		}
		seen[key] = true
		maximumAge, err := time.ParseDuration(target.MaximumAge)
		if err != nil {
			return err
		}
		ci.Targets[githubactions.CITargetKey{TenantID: "validation", Service: target.Service, Environment: target.Environment}] = githubactions.CITarget{TenantID: "validation", Owner: target.Owner, Repository: target.Repository, Workflow: target.Workflow, MaximumAge: maximumAge}
	}
	if err := ci.Validate(); err != nil {
		return err
	}
	seen = map[string]bool{}
	for _, m := range c.Metadata {
		key := m.Service + "\x00" + m.Environment
		age, err := time.ParseDuration(m.MaximumAge)
		if err != nil || age <= 0 || age > 24*time.Hour || strings.TrimSpace(m.Service) == "" || strings.TrimSpace(m.Environment) == "" || seen[key] || !metadataRepoPart.MatchString(m.Owner) || !metadataRepoPart.MatchString(m.Repository) || !commitSHA.MatchString(m.Revision) || !validMetadataPath(m.ContractPath) || !validMetadataPath(m.ProfilePath) {
			return fmt.Errorf("invalid trusted metadata source")
		}
		seen[key] = true
	}
	return nil
}
func (c AdmissionConfig) Trust(tenant string) (AdmissionTrust, error) {
	if c.Validate() != nil || c.Mode != "signed" || tenant == "" {
		return AdmissionTrust{}, ErrSourceContext
	}
	age, _ := time.ParseDuration(c.MaximumAge)
	depAge, _ := time.ParseDuration(c.DependencyMaximumAge)
	trust := AdmissionTrust{Issuer: c.Issuer, TenantID: tenant, BuildIssuer: c.BuildIssuer, MinimumRevision: c.MinimumRevision, MaximumAge: age, DependencyMaximumAge: depAge, Keys: grant.FileKeys{Path: c.TrustBundleFile, Issuer: c.Issuer, Tenant: tenant}, BuildKeys: grant.FileKeys{Path: c.BuildTrustBundleFile, Issuer: c.BuildIssuer, Tenant: tenant}, CI: map[DependencyKey]githubactions.CITarget{}, Metadata: map[DependencyKey]MetadataTarget{}}
	for _, target := range c.Targets {
		age, _ := time.ParseDuration(target.MaximumAge)
		trust.CI[DependencyKey{TenantID: tenant, Service: target.Service, Environment: target.Environment}] = githubactions.CITarget{TenantID: tenant, Owner: target.Owner, Repository: target.Repository, Workflow: target.Workflow, MaximumAge: age}
	}
	for _, target := range c.Metadata {
		age, _ := time.ParseDuration(target.MaximumAge)
		trust.Metadata[DependencyKey{TenantID: tenant, Service: target.Service, Environment: target.Environment}] = MetadataTarget{Owner: target.Owner, Repository: target.Repository, Revision: target.Revision, ContractPath: target.ContractPath, ProfilePath: target.ProfilePath, MaximumAge: age}
	}
	return trust, nil
}
