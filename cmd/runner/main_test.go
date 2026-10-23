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

	"go.yaml.in/yaml/v3"
	ecsadapter "themisy/adapters/ecs"
	"themisy/internal/config"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/pkg/credentials"
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
		{"pin without bundle", "production", controlPlaneSettings{TrustBundleHash: "sha256:invalid"}, "requires"},
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

func TestOIDCTrustConfigurationSeparatesProductionJWKSFromDevelopmentKey(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		settings identitySettings
		wantErr  string
	}{
		{name: "production discovery", mode: "production", settings: identitySettings{Issuer: "https://idp.example", Audience: "themisy", DiscoveryURL: "https://idp.example/.well-known/openid-configuration"}},
		{name: "production static refused", mode: "production", settings: identitySettings{Issuer: "https://idp.example", Audience: "themisy", TrustKeyID: "key", TrustKeyFile: "key.pub"}, wantErr: "requires trusted OIDC"},
		{name: "development static", mode: "development", settings: identitySettings{Issuer: "https://idp.example", Audience: "themisy", TrustKeyID: "key", TrustKeyFile: "key.pub"}},
		{name: "mixed refused", mode: "development", settings: identitySettings{Issuer: "https://idp.example", DiscoveryURL: "https://idp.example/discovery", TrustKeyID: "key", TrustKeyFile: "key.pub"}, wantErr: "not both"},
		{name: "http discovery refused", mode: "development", settings: identitySettings{Issuer: "https://idp.example", DiscoveryURL: "http://idp.example/discovery"}, wantErr: "https"},
		{name: "cache ttl bounded", mode: "production", settings: identitySettings{Issuer: "https://idp.example", DiscoveryURL: "https://idp.example/discovery", CacheTTL: "2h"}, wantErr: "cache TTL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateIdentityTrust(test.settings, test.mode)
			if test.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error=%v, want %q", err, test.wantErr)
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
	reportKey := writeSecret(t, directory, "report.key", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	configuration := settings{ResultSigning: config.GrantSigning{Provider: "development", KeyID: "runner-report", PrivateKeyFile: reportKey}, Mode: "development", RunnerID: "runner-1", RunnerGroup: "staging", TenantID: "tenant-1", ControlPlane: controlPlaneSettings{Address: "https://control.example", Issuer: "https://control.example", TokenFile: token, TrustKeyID: "grant-key", TrustKeyFile: grantKey}, Identity: identitySettings{Issuer: "https://idp.example", Audience: "themisy", TrustKeyID: "oidc-key", TrustKeyFile: oidcKey, DelegationsFile: delegations}, Capabilities: []protocol.Capability{protocol.CapabilityDeploy}, Credentials: credentialSettings{GitHubTokenFile: githubToken}, Adapters: adapterSettings{GitHubActions: githubActionsSettings{Targets: []githubTargetSettings{{Service: "payment-api", Environment: "staging", Owner: "example", Repository: "payment", DeployWorkflow: "deploy.yml", RollbackWorkflow: "rollback.yml", Ref: "main"}}}}}
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
	configuration.Identity.DiscoveryURL = "https://idp.example/.well-known/openid-configuration"
	configuration.Identity.CacheTTL = "10m"
	configuration.Identity.TrustKeyID, configuration.Identity.TrustKeyFile = "", ""
	discovered, _, _, cleanupDiscovery, err := buildRunner(context.Background(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupDiscovery()
	if _, ok := discovered.Subjects.(*identity.OIDCVerifier).Keys.(*identity.OIDCJWKSResolver); !ok {
		t.Fatalf("OIDC keys=%T, want OIDCJWKSResolver", discovered.Subjects.(*identity.OIDCVerifier).Keys)
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

func TestECSTargetsUseDistinctPurposeScopedRoles(t *testing.T) {
	configuration := settings{TenantID: "tenant-1", Adapters: adapterSettings{ECS: ecsSettings{Targets: []ecsTargetSettings{{
		Service: "payment-api", Environment: "production", Region: "ap-northeast-1",
		ClusterARN:      "arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod",
		ServiceARN:      "arn:aws:ecs:ap-northeast-1:111122223333:service/prod/payment-api",
		DeployRoleARN:   "arn:aws:iam::111122223333:role/payment-deploy",
		RollbackRoleARN: "arn:aws:iam::111122223333:role/payment-rollback",
		ExternalID:      "customer-id", RoleDuration: "20m",
		RollbackTaskDefinition: "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/payment-api:41",
	}}}}}
	targets, roles, err := buildECSTargets(configuration)
	if err != nil {
		t.Fatal(err)
	}
	key := credentials.AWSRoleKey{TenantID: "tenant-1", Service: "payment-api", Environment: "production"}
	deploy, deployOK := roles[credentials.AWSRoleKey{TenantID: key.TenantID, Service: key.Service, Environment: key.Environment, Purpose: credentials.PurposeDeploy}]
	rollback, rollbackOK := roles[credentials.AWSRoleKey{TenantID: key.TenantID, Service: key.Service, Environment: key.Environment, Purpose: credentials.PurposeRollback}]
	if !deployOK || !rollbackOK || deploy.RoleARN == rollback.RoleARN || deploy.RoleARN != configuration.Adapters.ECS.Targets[0].DeployRoleARN || rollback.RoleARN != configuration.Adapters.ECS.Targets[0].RollbackRoleARN {
		t.Fatalf("deploy role=%#v rollback role=%#v", deploy, rollback)
	}
	if deploy.ExternalID != "customer-id" || rollback.ExternalID != "customer-id" || deploy.Duration != 20*time.Minute || rollback.Duration != 20*time.Minute {
		t.Fatalf("deploy role=%#v rollback role=%#v", deploy, rollback)
	}
	if target := targets[ecsadapter.TargetKey{Service: key.Service, Environment: key.Environment}]; target.RollbackTaskDefinition != configuration.Adapters.ECS.Targets[0].RollbackTaskDefinition {
		t.Fatalf("rollback target=%#v", target)
	}
}

func TestECSTargetsRequireSeparateRollbackRole(t *testing.T) {
	configuration := settings{TenantID: "tenant-1", Adapters: adapterSettings{ECS: ecsSettings{Targets: []ecsTargetSettings{{
		Service: "payment-api", Environment: "production", Region: "ap-northeast-1",
		ClusterARN:             "arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod",
		ServiceARN:             "arn:aws:ecs:ap-northeast-1:111122223333:service/prod/payment-api",
		DeployRoleARN:          "arn:aws:iam::111122223333:role/payment-deploy",
		RollbackRoleARN:        "arn:aws:iam::111122223333:role/payment-deploy",
		RollbackTaskDefinition: "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/payment-api:41",
	}}}}}
	if _, _, err := buildECSTargets(configuration); err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("shared deploy/rollback role error=%v", err)
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
  trust_key_id: development-oidc
  trust_key_file: oidc.pub
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

func TestResultSignerRejectsMissingAndProductionDevelopmentKeys(t *testing.T) {
	for _, s := range []settings{{Mode: "development"}, {Mode: "production", ResultSigning: config.GrantSigning{Provider: "development", KeyID: "key", PrivateKeyFile: "must-not-load"}}, {Mode: "development", ResultSigning: config.GrantSigning{Provider: "unknown", KeyID: "key"}}, {Mode: "production", ResultSigning: config.GrantSigning{Provider: "aws-kms", KeyID: "key"}}} {
		if _, err := loadResultSigner(context.Background(), s); err == nil {
			t.Fatal("invalid result signer accepted")
		}
	}
}

func TestRunnerExampleIncludesValidResultSigner(t *testing.T) {
	t.Setenv("THEMISY_RESULT_PRIVATE_KEY_FILE", "/run/secrets/runner-result-key")
	t.Setenv("THEMISY_GRANT_PUBLIC_KEY_FILE", "/run/config/grant.pub")
	t.Setenv("THEMISY_OIDC_PUBLIC_KEY_FILE", "/run/config/oidc.pub")
	if _, err := load("../../config/runner.example.yaml"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../config/runner.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(string(raw), "  key_id: development-result-key", "  key_id: \"\"", 1)
	if _, err := load(writeConfig(t, invalid)); err == nil || !strings.Contains(err.Error(), "result_signing") {
		t.Fatalf("check-config accepted missing result signer: %v", err)
	}
}

func TestECSContainerDigestSettingsReachTrustedAdapter(t *testing.T) {
	deployDigest := "sha256:" + strings.Repeat("a", 64)
	rollbackDigest := "sha256:" + strings.Repeat("b", 64)
	var configuration settings
	data := `adapters:
  ecs:
    targets:
    - service: payment-api
      environment: production
      region: ap-northeast-1
      cluster_arn: arn:aws:ecs:ap-northeast-1:111122223333:cluster/prod
      service_arn: arn:aws:ecs:ap-northeast-1:111122223333:service/prod/payment-api
      deploy_role_arn: arn:aws:iam::111122223333:role/deploy
      rollback_role_arn: arn:aws:iam::111122223333:role/rollback
      rollback_task_definition: previous
      rollback_container_digests:
        app: ` + rollbackDigest + `
      task_definitions:
      - artifact_digest: ` + deployDigest + `
        task_definition_arn: next
        expected_task_definitions: [previous]
        expected_container_digests:
          app: ` + deployDigest + `
`
	if err := yaml.Unmarshal([]byte(data), &configuration); err != nil {
		t.Fatal(err)
	}
	targets, _, err := buildECSTargets(configuration)
	if err != nil {
		t.Fatal(err)
	}
	target := targets[ecsadapter.TargetKey{Service: "payment-api", Environment: "production"}]
	if target.RollbackContainerDigests["app"] != rollbackDigest || target.TaskDefinitions[deployDigest].ExpectedContainerDigests["app"] != deployDigest {
		t.Fatalf("lost expectations: %#v", target)
	}
	if err := (ecsadapter.Config{Targets: targets}).Validate(); err != nil {
		t.Fatal(err)
	}
}
