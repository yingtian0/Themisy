package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/pkg/protocol"
)

func TestRunnerConfigRejectsProductionWithoutDurableTrustBoundary(t *testing.T) {
	path := writeConfig(t, `
mode: production
runner_id: runner-1
runner_group: prod
tenant_id: tenant-1
control_plane:
  address: http://control.example
  issuer: https://control.example
identity:
  issuer: https://idp.example
  audience: themisy
policy:
  bundle_file: bundle.json
capabilities: [release.deploy]
`)
	if _, err := load(path); err == nil || !strings.Contains(err.Error(), "durable journal") {
		t.Fatalf("got %v", err)
	}
}

func TestGrantTrustConfigurationRequiresRefreshableProductionBundle(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		config controlPlaneSettings
		want   string
	}{
		{"production missing bundle", "production", controlPlaneSettings{}, "refreshable"},
		{"production legacy key", "production", controlPlaneSettings{TrustKeyID: "old", TrustKeyFile: "old.pub"}, "refreshable"},
		{"production bundle and legacy key", "production", controlPlaneSettings{TrustBundleFile: "bundle.json", TrustKeyID: "old", TrustKeyFile: "old.pub"}, "both"},
		{"production bundle", "production", controlPlaneSettings{TrustBundleFile: "bundle.json"}, ""},
		{"development bundle", "development", controlPlaneSettings{TrustBundleFile: "bundle.json"}, ""},
		{"development legacy key", "development", controlPlaneSettings{TrustKeyID: "local", TrustKeyFile: "local.pub"}, ""},
		{"development incomplete key", "development", controlPlaneSettings{TrustKeyID: "local"}, "together"},
		{"development no key", "development", controlPlaneSettings{}, "requires"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateGrantTrust(test.mode, test.config)
			if test.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestBuildRunnerComposesExecutableDependencies(t *testing.T) {
	directory := t.TempDir()
	grantPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oidcPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	grantKey := writeSecret(t, directory, "grant.pub", base64.RawURLEncoding.EncodeToString(grantPublic))
	oidcKey := writeSecret(t, directory, "oidc.pub", base64.RawURLEncoding.EncodeToString(oidcPublic))
	token := writeSecret(t, directory, "runner.token", "runner-token")
	githubToken := writeSecret(t, directory, "github.json", `{"token":"not-read-during-build","expires_at":"2099-01-01T00:00:00Z"}`)
	delegationData, _ := json.Marshal([]identity.Delegation{{ID: "delegation-1", Issuer: "https://idp.example", UserSubject: "user-1", AgentID: "agent-1", Actions: []protocol.Capability{protocol.CapabilityDeploy}, ServiceSelectors: []string{"payment-api"}, Environments: []string{"staging"}, MaximumRisk: "medium", ExpiresAt: time.Now().Add(time.Hour)}})
	delegations := writeSecret(t, directory, "delegations.json", string(delegationData))
	configuration := settings{Mode: "development", RunnerID: "runner-1", RunnerGroup: "staging", TenantID: "tenant-1", ControlPlane: controlPlaneSettings{Address: "https://control.example", Issuer: "https://control.example", TokenFile: token, TrustKeyID: "grant-key", TrustKeyFile: grantKey}, Identity: identitySettings{Issuer: "https://idp.example", Audience: "themisy", TrustKeyID: "oidc-key", TrustKeyFile: oidcKey, DelegationsFile: delegations}, Capabilities: []protocol.Capability{protocol.CapabilityDeploy}, Credentials: credentialSettings{GitHubTokenFile: githubToken}, Adapters: adapterSettings{GitHubActions: githubActionsSettings{Targets: []githubTargetSettings{{Service: "payment-api", Environment: "staging", Owner: "example", Repository: "payment", DeployWorkflow: "deploy.yml", RollbackWorkflow: "rollback.yml", Ref: "main"}}}}}
	executionRunner, client, reconciler, cleanup, err := buildRunner(context.Background(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if executionRunner.Grants == nil || executionRunner.Subjects == nil || executionRunner.Delegations == nil || executionRunner.Policy == nil || executionRunner.Journal == nil || executionRunner.Credentials == nil || executionRunner.Adapter == nil || executionRunner.Connectivity == nil || executionRunner.Admission == nil || client == nil || client.Runtime == nil || reconciler == nil {
		t.Fatalf("runner dependencies were not fully composed: %#v", executionRunner)
	}

	configuration.ControlPlane.TrustBundleFile = writeGrantBundleForTest(t, directory, runnerGrantTrustBundle(time.Now().UTC(), configuration.ControlPlane.Issuer, configuration.TenantID, "grant-key", grantPublic, "active"))
	configuration.ControlPlane.TrustKeyID, configuration.ControlPlane.TrustKeyFile = "", ""
	bundled, _, _, cleanupBundle, err := buildRunner(context.Background(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupBundle()
	if _, ok := bundled.Grants.(*grant.Verifier).Keys.(grant.FileKeys); !ok {
		t.Fatalf("Grant verifier keys=%T, want refreshable file resolver", bundled.Grants.(*grant.Verifier).Keys)
	}
}

func runnerGrantTrustBundle(now time.Time, issuer, tenant, keyID string, public ed25519.PublicKey, status string) grant.GrantTrustBundle {
	return grant.GrantTrustBundle{Version: "themisy.grant-trust/v1", Issuer: issuer, TenantID: tenant, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Keys: []grant.GrantTrustKey{{KeyID: keyID, PublicKey: base64.RawURLEncoding.EncodeToString(public), Status: status, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}}}
}

func writeGrantBundleForTest(t *testing.T, directory string, bundle grant.GrantTrustBundle) string {
	t.Helper()
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return writeSecret(t, directory, "grant-trust.json", string(data))
}

func writeSecret(t *testing.T, directory, name, value string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunnerConfigRejectsGenericCapability(t *testing.T) {
	path := writeConfig(t, `
mode: development
runner_id: runner-1
runner_group: dev
tenant_id: tenant-1
control_plane:
  address: https://control.example
  issuer: https://control.example
identity:
  issuer: https://idp.example
  audience: themisy
policy:
  bundle_file: bundle.json
capabilities: [cloud.generic]
`)
	if _, err := load(path); err == nil || !strings.Contains(err.Error(), "unsupported capability") {
		t.Fatalf("got %v", err)
	}
}

func TestRunnerConfigRejectsArbitraryWorkflowPath(t *testing.T) {
	path := writeConfig(t, `
mode: development
runner_id: runner-1
runner_group: dev
tenant_id: tenant-1
control_plane:
  address: https://control.example
  issuer: https://control.example
identity:
  issuer: https://idp.example
  audience: themisy
policy:
  bundle_file: bundle.json
adapters:
  github_actions:
    targets:
      - service: identity
        environment: staging
        owner: acme
        repository: releases
        deploy_workflow: ../../arbitrary.yml
        rollback_workflow: rollback.yml
        ref: main
capabilities: [release.deploy]
`)
	if _, err := load(path); err == nil || !strings.Contains(err.Error(), "workflow") {
		t.Fatalf("got %v", err)
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
