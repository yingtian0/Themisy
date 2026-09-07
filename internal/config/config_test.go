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
