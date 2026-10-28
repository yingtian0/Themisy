package contextsnapshot

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/adapter"
	"time"
)

type admissionHTTP struct {
	ci       *ciSourceHTTP
	metadata *metadataHTTP
}

func (h admissionHTTP) Do(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/actions/") {
		return h.ci.Do(r)
	}
	return h.metadata.Do(r)
}
func sourceTrustFile(t *testing.T, directory, name, issuer, keyID string, key ed25519.PublicKey, now time.Time) string {
	t.Helper()
	path := filepath.Join(directory, name)
	bundle := grant.GrantTrustBundle{Version: "themisy.grant-trust/v1", Issuer: issuer, TenantID: "tenant-a", IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Keys: []grant.GrantTrustKey{{KeyID: keyID, PublicKey: base64.RawURLEncoding.EncodeToString(key), Status: "active", NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}}}
	raw, _ := json.Marshal(bundle)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestAdmissionCollectorUsesRealReadersAndPublishesFailureWithoutPriorPass(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	directory := t.TempDir()
	_, token, ciHTTP, _, _, _ := ciCollectorFixture(t)
	token.expiry = now.Add(time.Hour)
	ciHTTP.runBody = strings.ReplaceAll(ciHTTP.runBody, "2026-10-19T23:59:00Z", now.Add(-time.Minute).Format(time.RFC3339))
	_, _, metadataHTTP, _, _, _, _ := metadataFixture(t)
	metadataHTTP.files["contracts/payment-api.yaml"] = regexp.MustCompile(`(?s)\ndependencies:.*?\nverification:`).ReplaceAllString(metadataHTTP.files["contracts/payment-api.yaml"], "\nverification:")
	sourceSigner, sourceKey, _ := grant.NewDevelopmentSigner("source-key")
	buildSigner, buildKey, _ := grant.NewDevelopmentSigner("build-key")
	sourceTrust := sourceTrustFile(t, directory, "source-trust.json", "source-issuer", "source-key", sourceKey, now)
	buildTrust := sourceTrustFile(t, directory, "build-trust.json", "build-issuer", "build-key", buildKey, now)
	build, err := SignBuildStatement(ctx, buildSigner, "build-key", BuildStatement{Service: "payment-api", Issuer: "build-issuer", TenantID: "tenant-a", Repository: "example/themisy-demo", CommitSHA: strings.Repeat("a", 40), Workflow: "ci.yml", RunID: 42, RunAttempt: 1, ArtifactDigest: "sha256:" + strings.Repeat("d", 64), IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	buildPath := filepath.Join(directory, "build.json")
	raw, _ := json.Marshal(build)
	os.WriteFile(buildPath, raw, 0600)
	cfg := AdmissionConfig{Mode: "signed", File: filepath.Join(directory, "sources.json"), Issuer: "source-issuer", TrustBundleFile: sourceTrust, BuildIssuer: "build-issuer", BuildTrustBundleFile: buildTrust, MinimumRevision: 1, MaximumAge: "5m", DependencyMaximumAge: "5m", Targets: []CISourceConfig{{Service: "payment-api", Environment: "staging", Owner: "example", Repository: "themisy-demo", Workflow: "ci.yml", MaximumAge: "1h", BuildStatementFile: buildPath}}, Metadata: []MetadataSourceConfig{{Service: "payment-api", Environment: "staging", Owner: "trusted-org", Repository: "config", Revision: strings.Repeat("b", 40), ContractPath: "contracts/payment-api.yaml", ProfilePath: "profiles/critical.yaml", MaximumAge: "1h"}}}
	sink := &ciAuditSink{}
	collector := &AdmissionCollector{Config: cfg, TenantID: "tenant-a", KeyID: "source-key", Revision: 1, Signer: sourceSigner, GitHub: token, GitHubHTTP: admissionHTTP{ci: ciHTTP, metadata: metadataHTTP}, Audit: sink}
	first, err := collector.Collect(ctx)
	if err != nil || first.Status != adapter.VerificationPass {
		t.Fatalf("status=%s err=%v", first.Status, err)
	}
	trust, err := cfg.Trust("tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	reader := &FileAdmissionSource{Path: cfg.File, Trust: trust, Ledger: &store.MemoryContextLedger{}, Audit: sink}
	write := func(b AdmissionBundle) {
		t.Helper()
		raw, _ := json.Marshal(b)
		if err := os.WriteFile(cfg.File, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(first)
	if _, err := reader.Read(ctx); err != nil {
		t.Fatal(err)
	}
	// The same real CI reader now sees an authoritative provider failure.
	ciHTTP.status = 503
	collector.Revision = 2
	failed, err := collector.Collect(ctx)
	if err != nil || failed.Status != adapter.VerificationInconclusive || len(failed.Targets) != 0 || len(failed.Observations) != 1 {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	write(failed)
	if _, err := reader.Read(ctx); err == nil {
		t.Fatal("provider failure authorized cached PASS")
	}
	write(first)
	if _, err := reader.Read(ctx); err == nil {
		t.Fatal("older PASS restored after published provider failure")
	}
}
