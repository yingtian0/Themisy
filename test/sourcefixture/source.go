// Package sourcefixture assembles real typed readers over synthetic HTTP sources.
// No production composition imports this test support package.
package sourcefixture

import (
	"context"
	"crypto/ed25519"
	"crypto/sha1"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"themisy/internal/contextsnapshot"
	"themisy/internal/grant"
	"themisy/pkg/credentials"
	"time"
)

//go:embed contract.yaml profile.yaml
var files embed.FS

type Fixture struct {
	Config                     contextsnapshot.AdmissionConfig
	Collector                  *contextsnapshot.AdmissionCollector
	Provider                   *provider
	TenantID, Artifact, Commit string
	Build                      contextsnapshot.BuildStatement
	BuildSigner                grant.Signer
	BuildPath                  string
}
type provider struct {
	Failure           atomic.Bool
	contract, profile string
	commit            string
}

func (p *provider) Do(r *http.Request) (*http.Response, error) {
	body := ""
	status := 200
	if strings.Contains(r.URL.Path, "/actions/") {
		if p.Failure.Load() {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
		}
		if strings.HasSuffix(r.URL.Path, "/runs") {
			body = fmt.Sprintf(`{"total_count":1,"workflow_runs":[{"id":42,"workflow_id":7,"head_sha":%q,"path":".github/workflows/ci.yml@main","status":"completed","conclusion":"success","run_attempt":1,"updated_at":%q}]}`, p.commit, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
		} else {
			body = fmt.Sprintf(`{"total_count":1,"artifacts":[{"id":81,"name":"artifact-archive","digest":"sha256:%s","expired":false,"workflow_run":{"id":42,"head_sha":%q}}]}`, strings.Repeat("a", 64), p.commit)
		}
	} else {
		name := "contracts/payment-api.yaml"
		raw := p.contract
		if strings.Contains(r.URL.Path, "profiles/") {
			name = "profiles/critical.yaml"
			raw = p.profile
		}
		blob := sha1.New()
		fmt.Fprintf(blob, "blob %d\x00", len(raw))
		blob.Write([]byte(raw))
		encoded, _ := json.Marshal(map[string]any{"type": "file", "path": name, "sha": hex.EncodeToString(blob.Sum(nil)), "encoding": "base64", "size": len(raw), "content": base64.StdEncoding.EncodeToString([]byte(raw))})
		body = string(encoded)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

type token struct{}

func (token) Token(context.Context) (credentials.Credential, error) {
	return credentials.New(map[string][]byte{"token": []byte("source-credential-canary")}, time.Now().Add(time.Hour)), nil
}
func trustFile(t testing.TB, directory, name, issuer, tenant, keyID string, key ed25519.PublicKey) string {
	t.Helper()
	now := time.Now().UTC()
	bundle := grant.GrantTrustBundle{Version: "themisy.grant-trust/v1", Issuer: issuer, TenantID: tenant, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Keys: []grant.GrantTrustKey{{KeyID: keyID, PublicKey: base64.RawURLEncoding.EncodeToString(key), Status: "active", NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}}}
	raw, _ := json.Marshal(bundle)
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func New(t testing.TB, sink contextsnapshot.CIAuditSink, tenants ...string) *Fixture {
	tenant := "tenant-1"
	if len(tenants) > 0 {
		tenant = tenants[0]
	}
	t.Helper()
	directory := t.TempDir()
	contract, _ := files.ReadFile("contract.yaml")
	profile, _ := files.ReadFile("profile.yaml")
	contract = regexp.MustCompile(`(?s)\ndependencies:.*?\nverification:`).ReplaceAll(contract, []byte("\nverification:"))
	f := &Fixture{TenantID: tenant, Artifact: "sha256:" + strings.Repeat("d", 64), Commit: strings.Repeat("a", 40), Provider: &provider{contract: string(contract), profile: string(profile), commit: strings.Repeat("a", 40)}}
	sourceSigner, sourceKey, _ := grant.NewDevelopmentSigner("source-key")
	buildSigner, buildKey, _ := grant.NewDevelopmentSigner("build-key")
	f.BuildSigner = buildSigner
	f.Build = contextsnapshot.BuildStatement{Service: "payment-api", Issuer: "build-issuer", TenantID: f.TenantID, Repository: "example/themisy-demo", CommitSHA: f.Commit, Workflow: "ci.yml", RunID: 42, RunAttempt: 1, ArtifactDigest: f.Artifact, IssuedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	f.BuildPath = filepath.Join(directory, "build.json")
	f.WriteBuild(t)
	f.Config = contextsnapshot.AdmissionConfig{Mode: "signed", File: filepath.Join(directory, "sources.json"), Issuer: "source-issuer", TrustBundleFile: trustFile(t, directory, "source-trust.json", "source-issuer", f.TenantID, "source-key", sourceKey), BuildIssuer: "build-issuer", BuildTrustBundleFile: trustFile(t, directory, "build-trust.json", "build-issuer", f.TenantID, "build-key", buildKey), MinimumRevision: 1, MaximumAge: "5m", DependencyMaximumAge: "5m", Targets: []contextsnapshot.CISourceConfig{{Service: "payment-api", Environment: "production", Owner: "example", Repository: "themisy-demo", Workflow: "ci.yml", MaximumAge: "1h", BuildStatementFile: f.BuildPath}}, Metadata: []contextsnapshot.MetadataSourceConfig{{Service: "payment-api", Environment: "production", Owner: "trusted-org", Repository: "config", Revision: strings.Repeat("b", 40), ContractPath: "contracts/payment-api.yaml", ProfilePath: "profiles/critical.yaml", MaximumAge: "1h"}}}
	f.Collector = &contextsnapshot.AdmissionCollector{Config: f.Config, TenantID: f.TenantID, KeyID: "source-key", Revision: 1, Signer: sourceSigner, GitHub: token{}, GitHubHTTP: f.Provider, Audit: sink}
	f.Publish(t, 1)
	return f
}
func (f *Fixture) WriteBuild(t testing.TB) {
	t.Helper()
	signed, err := contextsnapshot.SignBuildStatement(context.Background(), f.BuildSigner, "build-key", f.Build)
	if err != nil {
		t.Fatal(err)
	}
	f.Build = signed
	raw, _ := json.Marshal(signed)
	if err := os.WriteFile(f.BuildPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *Fixture) Publish(t testing.TB, revision int64) contextsnapshot.AdmissionBundle {
	t.Helper()
	f.Collector.Revision = revision
	b, err := f.Collector.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	if err := os.WriteFile(f.Config.File, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return b
}
func (f *Fixture) Source(t testing.TB, ledger contextsnapshot.ContextLedger, sink contextsnapshot.CIAuditSink) *contextsnapshot.FileAdmissionSource {
	t.Helper()
	trust, err := f.Config.Trust(f.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	return &contextsnapshot.FileAdmissionSource{Path: f.Config.File, Trust: trust, Ledger: ledger, Audit: sink}
}
