package config

import (
	"os"
	"testing"
)

func TestLoadRejectsUnknownConfigurationFields(t *testing.T) {
	path := t.TempDir() + "/themisy.yaml"
	if err := os.WriteFile(path, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestExampleConfigurationLoads(t *testing.T) {
	configuration, err := Load("../../config/themisy.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Mode != "all" || configuration.Database.URL == "" || configuration.Temporal.TaskQueue == "" {
		t.Fatalf("configuration=%#v", configuration)
	}
}

func TestRunnerWorkloadIdentityRequiresServerMTLS(t *testing.T) {
	configuration, err := Load("../../config/themisy.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	configuration.RunnerTransport.Registrations[0].TokenFile = ""
	configuration.RunnerTransport.Registrations[0].WorkloadIdentity = "spiffe://example.internal/themisy/runner-1"
	if err := configuration.Validate(); err == nil {
		t.Fatal("workload identity without server TLS was accepted")
	}
	configuration.HTTP.TLSCertFile = "/run/secrets/server.crt"
	configuration.HTTP.TLSKeyFile = "/run/secrets/server.key"
	configuration.RunnerTransport.ClientCAFile = "/run/secrets/runner-ca.crt"
	if err := configuration.Validate(); err != nil {
		t.Fatalf("valid mTLS configuration rejected: %v", err)
	}
	configuration.RunnerTransport.Registrations[0].WorkloadIdentity = "https://example.internal/runner-1"
	if err := configuration.Validate(); err == nil {
		t.Fatal("non-SPIFFE workload identity was accepted")
	}
}

func TestRunnerRegistrationRequiresDedicatedResultTrust(t *testing.T) {
	c, err := Load("../../config/themisy.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.RunnerTransport.Registrations[0].ResultTrustBundleFile = ""
	if c.Validate() == nil {
		t.Fatal("registration without dedicated result trust accepted")
	}
}

func TestKMSRejectsDevelopmentHeaders(t *testing.T) {
	c, err := Load("../../config/themisy.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.Grants.Signing.Provider = "aws-kms"
	c.Grants.Signing.AWSRegion = "ap-northeast-1"
	if c.Validate() == nil {
		t.Fatal("production signer accepted development identity")
	}
	c.Identity = Identity{Mode: "oidc", TenantID: c.RunnerTransport.Registrations[0].TenantID, Issuer: "https://idp.example", Audience: "themisy", DiscoveryURL: "https://idp.example/.well-known/openid-configuration", CacheTTL: "5m", TenantClaim: "organization", RoleClaim: "groups", RoleMappings: map[string][]string{"release-team": {"deployer"}}, DirectoryFile: "directory.json", DirectoryIssuer: "directory", DirectoryTrustBundleFile: "directory-trust.json", DirectoryMinimumRevision: 1, AttestationEndpoint: "https://identity.example/attest", AttestationIssuer: "https://identity.example", AttestationCAFile: "ca", AttestationClientCertFile: "cert", AttestationClientKeyFile: "key", AttestationTrustBundleFile: "identity-trust.json"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Identity.TenantID = "other"
	if c.Validate() == nil {
		t.Fatal("cross tenant runner registration accepted")
	}
}
