// contextcollector reads allow-listed customer sources and publishes a signed
// admission file. It never has deployment or Control Plane Grant authority.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"go.yaml.in/yaml/v3"
	"os"
	"path/filepath"
	"strings"
	"themisy/adapters/datadog"
	"themisy/internal/config"
	"themisy/internal/contextsnapshot"
	"themisy/internal/grant"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/credentials"
	"time"
)

type settings struct {
	Mode                  string                          `yaml:"mode"`
	TenantID              string                          `yaml:"tenant_id"`
	Source                contextsnapshot.AdmissionConfig `yaml:"source"`
	Signing               config.GrantSigning             `yaml:"signing"`
	GitHubTokenFile       string                          `yaml:"github_token_file"`
	DatadogCredentialFile string                          `yaml:"datadog_credential_file"`
	Dependencies          []dependencySet                 `yaml:"dependencies"`
	Health                []healthTarget                  `yaml:"health"`
}
type dependencySet struct {
	Service     string   `yaml:"service"`
	Environment string   `yaml:"environment"`
	Services    []string `yaml:"services"`
}
type healthTarget struct {
	Service       string  `yaml:"service"`
	Environment   string  `yaml:"environment"`
	Site          string  `yaml:"site"`
	Expression    string  `yaml:"expression"`
	Comparator    string  `yaml:"comparator"`
	Threshold     float64 `yaml:"threshold"`
	Aggregation   string  `yaml:"aggregation"`
	MinimumPoints int     `yaml:"minimum_points"`
	MaximumAge    string  `yaml:"maximum_age"`
	Window        string  `yaml:"window"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "", "customer source collector configuration")
	revision := flag.Int64("revision", 0, "serialized monotonic source publication revision")
	flag.Parse()
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var c settings
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return err
	}
	if c.Source.Validate() != nil || c.Source.Mode != "signed" || c.TenantID == "" || *revision < 1 || (c.Mode != "production" && c.Mode != "development") {
		return fmt.Errorf("explicit collector mode, tenant and signed Source configuration required")
	}
	if c.Mode == "production" && c.Signing.Provider != "aws-kms" {
		return fmt.Errorf("production collector requires independent KMS signing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var sink contextsnapshot.CIAuditSink
	if c.Mode == "production" {
		url := os.Getenv("THEMISY_CONTEXT_DATABASE_URL")
		if url == "" {
			return fmt.Errorf("THEMISY_CONTEXT_DATABASE_URL requires a customer Runner-schema audit database")
		}
		st, err := postgresstore.NewRunnerJournal(ctx, url)
		if err != nil {
			return err
		}
		defer st.Close()
		sink = st
	} else {
		sink = store.NewMemory()
	}
	var signer grant.Signer
	switch c.Signing.Provider {
	case "aws-kms":
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.Signing.AWSRegion))
		if err != nil {
			return err
		}
		signer, err = grant.NewAWSKMSSigner(kms.NewFromConfig(cfg), c.Signing.KeyID)
		if err != nil {
			return err
		}
	case "development":
		if c.Mode != "development" {
			return fmt.Errorf("development source signer forbidden")
		}
		signer, _, err = grant.LoadDevelopmentSigner(c.Signing.PrivateKeyFile, c.Signing.KeyID)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported source signer")
	}
	dependencies := contextsnapshot.DependencyConfig{Sets: map[contextsnapshot.DependencyKey][]string{}, Health: datadog.HealthConfig{Targets: map[datadog.HealthTargetKey]datadog.HealthTarget{}}}
	for _, set := range c.Dependencies {
		key := contextsnapshot.DependencyKey{TenantID: c.TenantID, Service: set.Service, Environment: set.Environment}
		if _, duplicate := dependencies.Sets[key]; duplicate {
			return fmt.Errorf("duplicate dependency set")
		}
		dependencies.Sets[key] = set.Services
	}
	for _, target := range c.Health {
		age, err := time.ParseDuration(target.MaximumAge)
		if err != nil {
			return err
		}
		window, err := time.ParseDuration(target.Window)
		if err != nil {
			return err
		}
		key := datadog.HealthTargetKey{TenantID: c.TenantID, Service: target.Service, Environment: target.Environment}
		if _, duplicate := dependencies.Health.Targets[key]; duplicate {
			return fmt.Errorf("duplicate dependency health target")
		}
		dependencies.Health.Targets[key] = datadog.HealthTarget{Site: datadog.Site(target.Site), Query: datadog.Query{Expression: target.Expression, Comparator: target.Comparator, Threshold: target.Threshold, Aggregation: target.Aggregation, MinimumPoints: target.MinimumPoints, MaximumAge: age}, Window: window}
	}
	collector := &contextsnapshot.AdmissionCollector{Config: c.Source, TenantID: c.TenantID, KeyID: c.Signing.KeyID, Revision: *revision, Signer: signer, GitHub: credentials.GitHubTokenFileSource{Path: c.GitHubTokenFile}, Datadog: credentials.DatadogCredentialFileSource{Path: c.DatadogCredentialFile}, Dependencies: dependencies, Audit: sink}
	b, err := collector.Collect(ctx)
	if err != nil {
		return err
	}
	if err := publish(c.Source.File, b); err != nil {
		return err
	}
	// Exit success means publication succeeded, including a negative observation;
	// consumers must inspect Status and never treat exit status as source health.
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"hash": b.Hash, "revision": b.Revision, "status": b.Status, "reason_code": b.ReasonCode})
}
func publish(path string, b contextsnapshot.AdmissionBundle) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".source-admission-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(b); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
