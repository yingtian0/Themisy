package contextsnapshot

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"themisy/adapters/datadog"
	"themisy/adapters/githubactions"
	"themisy/internal/grant"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"time"
)

// AdmissionCollector runs only in the customer source-reading environment.
// Provider credentials and source-signing authority never enter the dispatcher.
type AdmissionCollector struct {
	Config          AdmissionConfig
	TenantID, KeyID string
	Revision        int64
	Signer          grant.Signer
	GitHub          credentials.GitHubTokenSource
	Datadog         credentials.DatadogCredentialSource
	Dependencies    DependencyConfig
	GitHubHTTP      githubactions.HTTPDoer
	DatadogHTTP     datadog.HTTPDoer
	Audit           CIAuditSink
	Now             func() time.Time
}

func (c *AdmissionCollector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
func (c *AdmissionCollector) Collect(ctx context.Context) (AdmissionBundle, error) {
	if c == nil || c.Audit == nil || c.GitHub == nil || c.Signer == nil || c.Revision < 1 {
		return AdmissionBundle{}, ErrSourceContext
	}
	trust, err := c.Config.Trust(c.TenantID)
	if err != nil {
		return AdmissionBundle{}, err
	}
	metadataCollector, err := NewMetadataCollector(MetadataConfig{Targets: trust.Metadata}, c.GitHub, c.GitHubHTTP, c.Audit)
	if err != nil {
		return AdmissionBundle{}, err
	}
	ciConfig := githubactions.CIConfig{Targets: map[githubactions.CITargetKey]githubactions.CITarget{}}
	for key, target := range trust.CI {
		ciConfig.Targets[githubactions.CITargetKey{TenantID: key.TenantID, Service: key.Service, Environment: key.Environment}] = target
	}
	ciCollector, err := NewCICollector(ciConfig, c.GitHub, c.GitHubHTTP)
	if err != nil {
		return AdmissionBundle{}, err
	}
	auditedCI, err := NewAuditedCICollector(ciCollector, c.Audit)
	if err != nil {
		return AdmissionBundle{}, err
	}
	var dependencyCollector *DependencyCollector
	if len(c.Dependencies.Sets) > 0 {
		dependencyCollector, err = NewDependencyCollector(c.Dependencies, c.Datadog, c.DatadogHTTP, c.Audit)
		if err != nil {
			return AdmissionBundle{}, err
		}
	}
	assembler, err := NewSourceContextAssembler(c.Audit)
	if err != nil {
		return AdmissionBundle{}, err
	}
	metadata := map[DependencyKey]MetadataSnapshot{}
	catalog := []MetadataSnapshot{}
	observations := []ObservationRecord{}
	status := adapter.VerificationPass
	for key := range trust.Metadata {
		snapshot, err := metadataCollector.Collect(ctx, key)
		if err != nil {
			status = sourceWorst(status, adapter.VerificationMissing)
			continue
		}
		metadata[key] = snapshot
		catalog = append(catalog, snapshot)
	}
	sources := []SourceContextSnapshot{}
	builds := []BuildStatement{}
	for _, target := range c.Config.Targets {
		key := DependencyKey{TenantID: c.TenantID, Service: target.Service, Environment: target.Environment}
		observation := ObservationRecord{Service: target.Service, Environment: target.Environment}
		m, ok := metadata[key]
		if !ok {
			status = sourceWorst(status, adapter.VerificationMissing)
			observations = append(observations, observation)
			continue
		}
		record := m.Record()
		observation.Metadata = &record
		build, err := readBuildStatement(target.BuildStatementFile)
		if err != nil || VerifyBuildStatement(ctx, build, c.Config.BuildIssuer, c.TenantID, trust.BuildKeys, c.now()) != nil || build.Service != target.Service || build.Repository != target.Owner+"/"+target.Repository || build.Workflow != target.Workflow {
			status = sourceWorst(status, adapter.VerificationMissing)
			observations = append(observations, observation)
			continue
		}
		ci, err := auditedCI.Collect(ctx, githubactions.CIRequest{TenantID: c.TenantID, Target: adapter.Target{Service: target.Service, Environment: target.Environment}, CommitSHA: build.CommitSHA})
		if err != nil {
			status = sourceWorst(status, adapter.VerificationMissing)
			observations = append(observations, observation)
			continue
		}
		ciRecord := ci.Record()
		observation.CI = &ciRecord
		status = sourceWorst(status, ciRecord.Evidence.Status)
		var dependencies DependencySnapshot
		contract, err := m.Contract()
		if err != nil {
			status = sourceWorst(status, adapter.VerificationMissing)
			observations = append(observations, observation)
			continue
		}
		needed := false
		for _, d := range contract.Dependencies {
			needed = needed || d.Service != ""
		}
		if needed {
			if dependencyCollector == nil {
				status = sourceWorst(status, adapter.VerificationMissing)
				observations = append(observations, observation)
				continue
			}
			dependencies, err = dependencyCollector.Collect(ctx, key)
			if err != nil {
				status = sourceWorst(status, adapter.VerificationMissing)
				observations = append(observations, observation)
				continue
			}
			dependencyRecord := dependencies.Record()
			observation.Dependencies = &dependencyRecord
			for _, evidence := range dependencyRecord.Evidence {
				status = sourceWorst(status, evidence.Status)
			}
		}
		observations = append(observations, observation)
		snapshot, err := assembler.Assemble(ctx, ci, dependencies, m)
		if err != nil {
			if status == adapter.VerificationPass {
				status = adapter.VerificationMissing
			}
			continue
		}
		sources = append(sources, snapshot)
		builds = append(builds, build)
	}
	if status != adapter.VerificationPass || len(sources) != len(c.Config.Targets) {
		if status == adapter.VerificationPass {
			status = adapter.VerificationMissing
		}
		b := AdmissionBundle{Version: AdmissionVersion, Issuer: c.Config.Issuer, TenantID: c.TenantID, Revision: c.Revision, Status: status, ReasonCode: "SOURCE_COLLECTION_UNAVAILABLE", IssuedAt: c.now(), ExpiresAt: c.now().Add(time.Minute), Observations: observations}
		return signAdmission(ctx, c.Signer, c.KeyID, b)
	}
	maximumAge, _ := time.ParseDuration(c.Config.MaximumAge)
	return SignAdmission(ctx, c.Signer, c.KeyID, c.Config.Issuer, c.TenantID, c.Revision, maximumAge, sources, catalog, builds, c.now())
}
func readBuildStatement(path string) (BuildStatement, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return BuildStatement{}, ErrSourceContext
	}
	file, err := os.Open(path)
	if err != nil {
		return BuildStatement{}, ErrSourceContext
	}
	defer file.Close()
	var b BuildStatement
	decoder := json.NewDecoder(io.LimitReader(file, (64<<10)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&b) != nil || decoder.Decode(new(any)) != io.EOF {
		return BuildStatement{}, ErrSourceContext
	}
	return b, nil
}
func sourceWorst(a, b adapter.VerificationStatus) adapter.VerificationStatus {
	weights := map[adapter.VerificationStatus]int{adapter.VerificationPass: 1, adapter.VerificationInconclusive: 2, adapter.VerificationMissing: 3, adapter.VerificationFail: 4}
	if !b.Valid() {
		b = adapter.VerificationMissing
	}
	if weights[b] > weights[a] {
		return b
	}
	return a
}
