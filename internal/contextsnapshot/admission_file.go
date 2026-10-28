package contextsnapshot

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"themisy/adapters/githubactions"
	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/profile"
	"themisy/pkg/adapter"
	"time"
)

type ContextLedger interface {
	AcceptContextRevision(context.Context, string, string, int64, string) error
}
type AdmissionTrust struct {
	Issuer, TenantID, BuildIssuer    string
	MinimumRevision                  int64
	MaximumAge, DependencyMaximumAge time.Duration
	Keys, BuildKeys                  grant.KeyResolver
	CI                               map[DependencyKey]githubactions.CITarget
	Metadata                         map[DependencyKey]MetadataTarget
}
type FileAdmissionSource struct {
	Path   string
	Trust  AdmissionTrust
	Ledger ContextLedger
	Audit  CIAuditSink
	Now    func() time.Time
}

func (f *FileAdmissionSource) now() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}
func cloneAdmission(b AdmissionBundle) AdmissionBundle {
	raw, _ := json.Marshal(b)
	var copy AdmissionBundle
	json.Unmarshal(raw, &copy)
	return copy
}
func (f *FileAdmissionSource) Read(ctx context.Context) (VerifiedAdmission, error) {
	if f == nil || f.Path == "" || f.Ledger == nil || f.Audit == nil || f.Trust.MinimumRevision < 1 || f.Trust.MaximumAge <= 0 || f.Trust.MaximumAge > 15*time.Minute || f.Trust.BuildIssuer == "" || f.Trust.BuildIssuer == f.Trust.Issuer || f.Trust.BuildKeys == nil || len(f.Trust.CI) == 0 || len(f.Trust.Metadata) == 0 || ctx.Err() != nil {
		return VerifiedAdmission{}, ErrSourceContext
	}
	info, err := os.Lstat(f.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 8<<20 {
		return VerifiedAdmission{}, ErrSourceContext
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return VerifiedAdmission{}, ErrSourceContext
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return VerifiedAdmission{}, ErrSourceContext
	}
	var b AdmissionBundle
	decoder := json.NewDecoder(io.LimitReader(file, (8<<20)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&b) != nil || decoder.Decode(new(any)) != io.EOF {
		return VerifiedAdmission{}, ErrSourceContext
	}
	if b.Revision < f.Trust.MinimumRevision || verifyAdmissionSignature(ctx, b, f.Trust.Issuer, f.Trust.TenantID, f.Trust.Keys, f.now()) != nil {
		return VerifiedAdmission{}, ErrSourceContext
	}
	// Even a signed negative/expired latest observation fences an earlier PASS.
	if f.Ledger.AcceptContextRevision(ctx, b.TenantID, b.Issuer, b.Revision, b.Hash) != nil {
		return VerifiedAdmission{}, ErrSourceContext
	}
	verified, verificationErr := f.verifyContents(ctx, b, f.now())
	result, reason := "VERIFIED", "SOURCE_ADMISSION_VERIFIED"
	if verificationErr != nil {
		result, reason = "REJECTED", "SOURCE_ADMISSION_UNAVAILABLE"
	}
	event := domain.AuditEvent{ID: "context_admission_" + rand.Text(), CorrelationID: "context/" + b.TenantID + "/" + b.Hash, ActorType: "system", ActorID: "source-admission", Action: "context.admission.verify", ResourceType: "source_context", ResourceID: b.Hash, Result: result, Timestamp: f.now(), Details: map[string]any{"issuer": b.Issuer, "tenant_id": b.TenantID, "revision": b.Revision, "hash": b.Hash, "status": b.Status, "reason_code": reason, "issued_at": b.IssuedAt, "expires_at": b.ExpiresAt, "sources": publicAdmissionRecord(b)}}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if f.Audit.AppendAuditContext(bounded, event) != nil || bounded.Err() != nil || verificationErr != nil {
		return VerifiedAdmission{}, ErrSourceContext
	}
	if _, err := f.verifyContents(ctx, b, f.now()); err != nil || verifyAdmissionSignature(ctx, b, f.Trust.Issuer, f.Trust.TenantID, f.Trust.Keys, f.now()) != nil {
		return VerifiedAdmission{}, ErrSourceContext
	}
	return verified, nil
}
func publicAdmissionRecord(b AdmissionBundle) any {
	records := []any{}
	for _, target := range b.Targets {
		records = append(records, map[string]any{"source": target.Source, "build": target.Build})
	}
	catalog := []MetadataRecord{}
	for _, metadata := range b.Catalog {
		catalog = append(catalog, metadata.Record)
	}
	return map[string]any{"targets": records, "catalog": catalog, "negative_observations": b.Observations}
}
func (f *FileAdmissionSource) verifyContents(ctx context.Context, b AdmissionBundle, now time.Time) (VerifiedAdmission, error) {
	if ctx.Err() != nil || b.Status != adapter.VerificationPass || len(b.Observations) > 0 || b.ReasonCode != "SOURCES_VERIFIED" || !b.ExpiresAt.After(now) || !b.IssuedAt.Add(f.Trust.MaximumAge).After(now) || b.ExpiresAt.After(b.IssuedAt.Add(f.Trust.MaximumAge)) || len(b.Catalog) != len(f.Trust.Metadata) || len(b.Catalog) > 128 || len(b.Targets) == 0 || len(b.Targets) > 128 {
		return VerifiedAdmission{}, ErrSourceContext
	}
	metadata := map[DependencyKey]MetadataSnapshot{}
	contracts := map[string]contract.ServiceContract{}
	profiles := map[string]profile.ReleaseProfile{}
	for _, item := range b.Catalog {
		r := item.Record
		key := DependencyKey{TenantID: b.TenantID, Service: r.Service, Environment: r.Environment}
		configuration, ok := f.Trust.Metadata[key]
		if !ok || r.TenantID != b.TenantID || metadata[key].Hash() != "" {
			return VerifiedAdmission{}, ErrSourceContext
		}
		m, err := restoreMetadata(item, configuration, now)
		if err != nil {
			return VerifiedAdmission{}, err
		}
		metadata[key] = m
		c, err := m.Contract()
		if err != nil || c.Metadata.Name != r.Service || r.Contract.Name != c.Metadata.Name || r.Contract.SchemaVersion != c.APIVersion || r.Contract.Kind != c.Kind {
			return VerifiedAdmission{}, ErrSourceContext
		}
		p, err := m.Profile()
		if err != nil || r.Profile.Name != p.Metadata.Name || r.Profile.SchemaVersion != p.APIVersion || r.Profile.Kind != p.Kind || c.Environments[r.Environment].ReleaseProfile != p.Metadata.Name {
			return VerifiedAdmission{}, ErrSourceContext
		}
		if old, ok := contracts[c.Metadata.Name]; ok && old.ContentHash != c.ContentHash {
			return VerifiedAdmission{}, ErrSourceContext
		}
		contracts[c.Metadata.Name] = c
		if old, ok := profiles[p.Metadata.Name]; ok && old.ContentHash != p.ContentHash {
			return VerifiedAdmission{}, ErrSourceContext
		}
		profiles[p.Metadata.Name] = p
		for _, entry := range []MetadataItem{r.Contract, r.Profile} {
			if entry.FetchedAt.After(b.IssuedAt) || b.ExpiresAt.After(entry.FetchedAt.Add(entry.MaximumAge)) {
				return VerifiedAdmission{}, ErrSourceContext
			}
		}
	}
	seen := map[DependencyKey]bool{}
	for _, target := range b.Targets {
		r := target.Source
		key := DependencyKey{TenantID: b.TenantID, Service: r.Target.Service, Environment: r.Target.Environment}
		configuration, ok := f.Trust.CI[key]
		if !ok || seen[key] || r.TenantID != b.TenantID {
			return VerifiedAdmission{}, ErrSourceContext
		}
		seen[key] = true
		if r.CI.Evidence.Repository != configuration.Owner+"/"+configuration.Repository || r.CI.Evidence.Workflow != configuration.Workflow || r.CI.Evidence.MaximumAge > configuration.MaximumAge || r.CI.Evidence.Conclusion != "success" || r.CapturedAt.After(b.IssuedAt) || b.ExpiresAt.After(r.ExpiresAt) {
			return VerifiedAdmission{}, ErrSourceContext
		}
		m, ok := metadata[key]
		if !ok || r.Metadata.Hash != m.Hash() {
			return VerifiedAdmission{}, ErrSourceContext
		}
		deps := DependencySnapshot{}
		if r.Dependencies != nil {
			deps.record = *r.Dependencies
			for _, evidence := range deps.record.Evidence {
				if f.Trust.DependencyMaximumAge <= 0 || evidence.MaximumAge > f.Trust.DependencyMaximumAge {
					return VerifiedAdmission{}, ErrSourceContext
				}
			}
		}
		rebuilt, err := composeSources(r.CapturedAt, CISnapshot{record: r.CI}, deps, m)
		if err != nil || rebuilt.Hash() != r.Hash || rebuilt.RequireFreshSources(now) != nil {
			return VerifiedAdmission{}, ErrSourceContext
		}
		build := target.Build
		if build.Service != r.Target.Service || build.IssuedAt.After(b.IssuedAt) || VerifyBuildStatement(ctx, build, f.Trust.BuildIssuer, b.TenantID, f.Trust.BuildKeys, now) != nil || build.Repository != r.CI.Evidence.Repository || build.CommitSHA != r.CommitSHA || build.Workflow != r.CI.Evidence.Workflow || build.RunID != r.CI.Evidence.RunID || build.RunAttempt != r.CI.Evidence.RunAttempt || b.ExpiresAt.After(build.ExpiresAt) {
			return VerifiedAdmission{}, ErrSourceContext
		}
	}
	verified := VerifiedAdmission{bundle: cloneAdmission(b)}
	for _, c := range contracts {
		verified.contracts = append(verified.contracts, c)
	}
	for _, p := range profiles {
		verified.profiles = append(verified.profiles, p)
	}
	return verified, nil
}
func (v VerifiedAdmission) Target(service, environment, commit, artifact string) (AdmissionTarget, error) {
	for _, target := range v.bundle.Targets {
		if target.Source.Target.Service == service && target.Source.Target.Environment == environment && (commit == "" || target.Source.CommitSHA == commit) && target.Build.ArtifactDigest == artifact {
			owned := target
			owned.Source.DependencyServices = append([]string(nil), target.Source.DependencyServices...)
			owned.Source.CI.Evidence.Artifacts = append([]githubactions.CIArtifact(nil), target.Source.CI.Evidence.Artifacts...)
			if target.Source.Dependencies != nil {
				copy := *target.Source.Dependencies
				copy.Evidence = append(copy.Evidence[:0:0], target.Source.Dependencies.Evidence...)
				owned.Source.Dependencies = &copy
			}
			return owned, nil
		}
	}
	return AdmissionTarget{}, ErrSourceContext
}
func (v *VerifiedAdmission) UnmarshalJSON([]byte) error {
	if v != nil {
		*v = VerifiedAdmission{}
	}
	return ErrSourceContext
}
