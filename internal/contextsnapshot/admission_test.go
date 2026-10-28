package contextsnapshot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"themisy/adapters/githubactions"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/adapter"
	"time"
)

func TestSignedAdmissionBindsActualArtifactAndFencesNegativeObservation(t *testing.T) {
	ctx := context.Background()
	ci, deps, metadata := sourceContextParts(t, true)
	sink := &ciAuditSink{}
	assembler, _ := NewSourceContextAssembler(sink)
	snapshot, err := assembler.Assemble(ctx, ci, deps, metadata)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sourceSigner, sourceKey, _ := grant.NewDevelopmentSigner("source-key")
	buildSigner, buildKey, _ := grant.NewDevelopmentSigner("build-key")
	e := ci.Record().Evidence
	build, err := SignBuildStatement(ctx, buildSigner, "build-key", BuildStatement{Service: ci.Record().Target.Service, Issuer: "build-issuer", TenantID: "tenant-a", Repository: e.Repository, CommitSHA: e.CommitSHA, Workflow: e.Workflow, RunID: e.RunID, RunAttempt: e.RunAttempt, ArtifactDigest: "sha256:" + strings.Repeat("d", 64), IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignAdmission(ctx, sourceSigner, "source-key", "source-issuer", "tenant-a", 1, time.Minute, []SourceContextSnapshot{snapshot}, []MetadataSnapshot{metadata}, []BuildStatement{build}, now)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := SignAdmission(ctx, sourceSigner, "source-key", "source-issuer", "tenant-a", 1, time.Minute, []SourceContextSnapshot{snapshot}, []MetadataSnapshot{metadata}, []BuildStatement{build}, now)
	if err != nil || signed.Hash != repeated.Hash {
		t.Fatal("identical source was not deterministic")
	}
	r := metadata.Record()
	parts := strings.Split(r.Contract.Repository, "/")
	key := DependencyKey{TenantID: "tenant-a", Service: r.Service, Environment: r.Environment}
	source := &FileAdmissionSource{Path: filepath.Join(t.TempDir(), "sources.json"), Ledger: &store.MemoryContextLedger{}, Audit: sink, Now: func() time.Time { return now }, Trust: AdmissionTrust{Issuer: "source-issuer", TenantID: "tenant-a", BuildIssuer: "build-issuer", MinimumRevision: 1, MaximumAge: time.Minute, DependencyMaximumAge: time.Hour, Keys: grant.StaticKeys{"source-issuer\x00source-key": sourceKey}, BuildKeys: grant.StaticKeys{"build-issuer\x00build-key": buildKey}, CI: map[DependencyKey]githubactions.CITarget{key: {TenantID: "tenant-a", Owner: "example", Repository: "themisy-demo", Workflow: e.Workflow, MaximumAge: time.Hour}}, Metadata: map[DependencyKey]MetadataTarget{key: {Owner: parts[0], Repository: parts[1], Revision: r.Contract.Revision, ContractPath: r.Contract.Path, ProfilePath: r.Profile.Path, MaximumAge: time.Hour}}}}
	write := func(b AdmissionBundle) {
		t.Helper()
		raw, _ := json.Marshal(b)
		if err := os.WriteFile(source.Path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(signed)
	verified, err := source.Read(ctx)
	if err != nil || verified.Hash() != signed.Hash {
		t.Fatalf("verified hash=%s err=%v", verified.Hash(), err)
	}
	if _, err := verified.Target(r.Service, r.Environment, e.CommitSHA, build.ArtifactDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := verified.Target(r.Service, r.Environment, e.CommitSHA, e.Artifacts[0].Digest); err == nil {
		t.Fatal("archive digest treated as deployment provenance")
	}
	for _, test := range []struct {
		name   string
		mutate func(*BuildStatement)
	}{
		{"repository", func(b *BuildStatement) { b.Repository = "other/repository" }},
		{"commit", func(b *BuildStatement) { b.CommitSHA = strings.Repeat("f", 40) }},
		{"workflow", func(b *BuildStatement) { b.Workflow = "other.yml" }},
		{"run", func(b *BuildStatement) { b.RunID++ }},
		{"attempt", func(b *BuildStatement) { b.RunAttempt++ }},
		{"service", func(b *BuildStatement) { b.Service = "other-service" }},
	} {
		t.Run("independent-build-mismatch-"+test.name, func(t *testing.T) {
			mismatched := cloneAdmission(signed)
			test.mutate(&mismatched.Targets[0].Build)
			var err error
			mismatched.Targets[0].Build, err = SignBuildStatement(ctx, buildSigner, "build-key", mismatched.Targets[0].Build)
			if err != nil {
				t.Fatal(err)
			}
			mismatched, err = signAdmission(ctx, sourceSigner, "source-key", mismatched)
			if err != nil {
				t.Fatal(err)
			}
			isolated := *source
			isolated.Ledger = &store.MemoryContextLedger{}
			write(mismatched)
			if _, err := isolated.Read(ctx); err == nil {
				t.Fatal("unrelated build proof authorized source")
			}
		})
	}
	write(signed)
	raw, _ := json.Marshal(signed)
	var dto VerifiedAdmission
	if json.Unmarshal(raw, &dto) == nil {
		t.Fatal("plain DTO restored trusted authority")
	}
	wrong := cloneAdmission(signed)
	wrong.Targets[0].Build.ArtifactDigest = "sha256:" + strings.Repeat("e", 64)
	wrong.Revision = 2
	wrong, err = signAdmission(ctx, sourceSigner, "source-key", wrong)
	if err != nil {
		t.Fatal(err)
	}
	write(wrong)
	if _, err := source.Read(ctx); err == nil {
		t.Fatal("context signer forged independent artifact provenance")
	}
	// Revision 2 was signed but invalid, so even a still-fresh revision 1 cannot
	// restore the older positive authority after this known change.
	write(signed)
	if _, err := source.Read(ctx); err == nil {
		t.Fatal("invalid latest context rolled back to old PASS")
	}
	negative, err := SignUnavailableAdmission(ctx, sourceSigner, "source-key", "source-issuer", "tenant-a", 3, adapter.VerificationMissing, now)
	if err != nil {
		t.Fatal(err)
	}
	write(negative)
	if _, err := source.Read(ctx); err == nil {
		t.Fatal("negative observation authorized")
	}
	signed.Revision = 4
	signed, err = signAdmission(ctx, sourceSigner, "source-key", signed)
	if err != nil {
		t.Fatal(err)
	}
	write(signed)
	if _, err := source.Read(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := source.Read(ctx); err == nil {
		t.Fatal("expired source reused")
	}
}
