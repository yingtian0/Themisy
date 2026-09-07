package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Mode            string          `yaml:"mode"`
	HTTP            HTTP            `yaml:"http"`
	Database        Database        `yaml:"database"`
	Temporal        Temporal        `yaml:"temporal"`
	Planning        Planning        `yaml:"planning"`
	Grants          Grants          `yaml:"grants"`
	RunnerTransport RunnerTransport `yaml:"runner_transport"`
}

type HTTP struct {
	Address     string `yaml:"address"`
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
}
type Database struct {
	URL         string `yaml:"url"`
	AutoMigrate bool   `yaml:"auto_migrate"`
}
type Temporal struct {
	Address   string `yaml:"address"`
	Namespace string `yaml:"namespace"`
	TaskQueue string `yaml:"task_queue"`
}
type Planning struct {
	Contracts     string `yaml:"contracts"`
	Profiles      string `yaml:"profiles"`
	LegacyCatalog string `yaml:"legacy_catalog"`
}
type Grants struct {
	Issuer  string       `yaml:"issuer"`
	TTL     string       `yaml:"ttl"`
	Signing GrantSigning `yaml:"signing"`
}
type GrantSigning struct {
	Provider       string `yaml:"provider"`
	KeyID          string `yaml:"key_id"`
	PrivateKeyFile string `yaml:"private_key_file"`
	AWSRegion      string `yaml:"aws_region"`
}
type RunnerTransport struct {
	ClientCAFile  string               `yaml:"client_ca_file"`
	Registrations []RunnerRegistration `yaml:"registrations"`
}
type RunnerRegistration struct {
	RunnerID         string `yaml:"runner_id"`
	TenantID         string `yaml:"tenant_id"`
	RunnerGroup      string `yaml:"runner_group"`
	WorkloadIdentity string `yaml:"workload_identity"`
	TokenFile        string `yaml:"token_file"`
}

func Load(path string) (Config, error) {
	result := Config{Mode: "all", HTTP: HTTP{Address: ":8080"}, Database: Database{AutoMigrate: true}, Temporal: Temporal{Address: "localhost:7233", Namespace: "default", TaskQueue: "themisy-releases"}, Planning: Planning{Contracts: "examples/contracts", Profiles: "examples/profiles"}}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read Themisy config: %w", err)
		}
		decoder := yaml.NewDecoder(strings.NewReader(string(data)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&result); err != nil {
			return Config{}, fmt.Errorf("decode Themisy config: %w", err)
		}
	}
	applyEnv(&result)
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (c Config) Validate() error {
	switch c.Mode {
	case "control", "worker", "all":
	default:
		return fmt.Errorf("mode must be control, worker, or all")
	}
	if c.Database.URL == "" {
		return fmt.Errorf("database.url is required")
	}
	if c.Temporal.Address == "" || c.Temporal.Namespace == "" || c.Temporal.TaskQueue == "" {
		return fmt.Errorf("temporal address, namespace, and task_queue are required")
	}
	if c.Planning.LegacyCatalog == "" && (c.Planning.Contracts == "" || c.Planning.Profiles == "") {
		return fmt.Errorf("planning contract and profile paths are required")
	}
	if c.Grants.Issuer == "" {
		return fmt.Errorf("grants.issuer is required")
	}
	ttl, err := time.ParseDuration(c.Grants.TTL)
	if err != nil || ttl <= 0 || ttl > 15*time.Minute {
		return fmt.Errorf("grants.ttl must be a duration between zero and fifteen minutes")
	}
	if c.Grants.Signing.KeyID == "" || (c.Grants.Signing.Provider != "development" && c.Grants.Signing.Provider != "aws-kms") {
		return fmt.Errorf("grant signing provider and key_id are required")
	}
	if c.Grants.Signing.Provider == "development" && c.Grants.Signing.PrivateKeyFile == "" {
		return fmt.Errorf("development grant signing requires private_key_file")
	}
	if c.Grants.Signing.Provider == "aws-kms" && c.Grants.Signing.AWSRegion == "" {
		return fmt.Errorf("AWS KMS grant signing requires aws_region")
	}
	if c.Mode != "worker" && len(c.RunnerTransport.Registrations) == 0 {
		return fmt.Errorf("control mode requires at least one runner registration")
	}
	seen := make(map[string]struct{}, len(c.RunnerTransport.Registrations))
	hasWorkloadIdentity := false
	for _, registration := range c.RunnerTransport.Registrations {
		if registration.RunnerID == "" || registration.TenantID == "" || registration.RunnerGroup == "" || (registration.TokenFile == "" && registration.WorkloadIdentity == "") {
			return fmt.Errorf("runner registration is incomplete")
		}
		if _, duplicate := seen[registration.RunnerID]; duplicate {
			return fmt.Errorf("duplicate runner registration %q", registration.RunnerID)
		}
		seen[registration.RunnerID] = struct{}{}
		hasWorkloadIdentity = hasWorkloadIdentity || registration.WorkloadIdentity != ""
		if registration.WorkloadIdentity != "" {
			identity, err := url.Parse(registration.WorkloadIdentity)
			if err != nil || identity.Scheme != "spiffe" || identity.Host == "" || identity.Path == "" {
				return fmt.Errorf("runner workload_identity must be an absolute SPIFFE URI")
			}
		}
	}
	if c.Mode != "worker" && hasWorkloadIdentity && (c.RunnerTransport.ClientCAFile == "" || c.HTTP.TLSCertFile == "" || c.HTTP.TLSKeyFile == "") {
		return fmt.Errorf("Runner mTLS requires http.tls_cert_file and http.tls_key_file")
	}
	return nil
}

func applyEnv(c *Config) {
	setString("THEMISY_MODE", &c.Mode)
	setString("THEMISY_HTTP_ADDRESS", &c.HTTP.Address)
	setString("THEMISY_HTTP_TLS_CERT_FILE", &c.HTTP.TLSCertFile)
	setString("THEMISY_HTTP_TLS_KEY_FILE", &c.HTTP.TLSKeyFile)
	setString("THEMISY_DATABASE_URL", &c.Database.URL)
	setString("THEMISY_TEMPORAL_ADDRESS", &c.Temporal.Address)
	setString("THEMISY_TEMPORAL_NAMESPACE", &c.Temporal.Namespace)
	setString("THEMISY_TEMPORAL_TASK_QUEUE", &c.Temporal.TaskQueue)
	setString("THEMISY_CONTRACTS", &c.Planning.Contracts)
	setString("THEMISY_PROFILES", &c.Planning.Profiles)
	setString("THEMISY_LEGACY_CATALOG", &c.Planning.LegacyCatalog)
	setString("THEMISY_GRANT_ISSUER", &c.Grants.Issuer)
	setString("THEMISY_GRANT_SIGNING_KEY_ID", &c.Grants.Signing.KeyID)
	setString("THEMISY_GRANT_SIGNING_PRIVATE_KEY_FILE", &c.Grants.Signing.PrivateKeyFile)
	setString("THEMISY_RUNNER_CLIENT_CA_FILE", &c.RunnerTransport.ClientCAFile)
}
func setString(name string, target *string) {
	if value, ok := os.LookupEnv(name); ok {
		*target = value
	}
}
