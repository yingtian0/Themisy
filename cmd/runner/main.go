package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"themisy/adapters/datadog"
	ecsadapter "themisy/adapters/ecs"
	"themisy/adapters/githubactions"
	"themisy/internal/application"
	"themisy/internal/config"
	"themisy/internal/contextsnapshot"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/policy"
	"themisy/internal/resultreport"
	runnercore "themisy/internal/runner"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	runnertransport "themisy/internal/transport"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"go.yaml.in/yaml/v3"
)

type settings struct {
	Context       contextsnapshot.AdmissionConfig `yaml:"context"`
	ResultSigning config.GrantSigning             `yaml:"result_signing"`
	Mode          string                          `yaml:"mode"`
	RunnerID      string                          `yaml:"runner_id"`
	RunnerGroup   string                          `yaml:"runner_group"`
	TenantID      string                          `yaml:"tenant_id"`
	Capacity      int                             `yaml:"capacity"`
	HealthAddress string                          `yaml:"health_address"`
	ControlPlane  controlPlaneSettings            `yaml:"control_plane"`
	Identity      identitySettings                `yaml:"identity"`
	Policy        policySettings                  `yaml:"policy"`
	Journal       journalSettings                 `yaml:"journal"`
	Manifests     manifestSettings                `yaml:"manifests"`
	Adapters      adapterSettings                 `yaml:"adapters"`
	Credentials   credentialSettings              `yaml:"credentials"`
	Capabilities  []protocol.Capability           `yaml:"capabilities"`
}
type controlPlaneSettings struct {
	Address         string `yaml:"address"`
	Issuer          string `yaml:"issuer"`
	TokenFile       string `yaml:"token_file"`
	ClientCertFile  string `yaml:"client_cert_file"`
	ClientKeyFile   string `yaml:"client_key_file"`
	CAFile          string `yaml:"ca_file"`
	TrustBundleFile string `yaml:"trust_bundle_file"`
	TrustBundleHash string `yaml:"trust_bundle_hash"`
	TrustKeyID      string `yaml:"trust_key_id"`
	TrustKeyFile    string `yaml:"trust_key_file"`
}
type identitySettings struct {
	AttestationIssuer          string `yaml:"attestation_issuer"`
	AttestationTrustBundleFile string `yaml:"attestation_trust_bundle_file"`
	DirectoryFile              string `yaml:"directory_file"`
	DirectoryIssuer            string `yaml:"directory_issuer"`
	DirectoryTrustBundleFile   string `yaml:"directory_trust_bundle_file"`
	DirectoryMinimumRevision   int64  `yaml:"directory_minimum_revision"`

	Issuer          string `yaml:"issuer"`
	Audience        string `yaml:"audience"`
	DiscoveryURL    string `yaml:"discovery_url"`
	CacheTTL        string `yaml:"jwks_cache_ttl"`
	TrustKeyID      string `yaml:"trust_key_id"`
	TrustKeyFile    string `yaml:"trust_key_file"`
	DelegationsFile string `yaml:"delegations_file"`
}
type policySettings struct {
	Issuer          string `yaml:"issuer"`
	TrustBundleFile string `yaml:"trust_bundle_file"`
	BundleFile      string `yaml:"bundle_file"`
	TrustKeyID      string `yaml:"trust_key_id"`
	TrustKeyFile    string `yaml:"trust_key_file"`
}
type journalSettings struct {
	DatabaseURL string `yaml:"database_url"`
}
type manifestSettings struct {
	File         string `yaml:"file"`
	Issuer       string `yaml:"issuer"`
	TrustKeyID   string `yaml:"trust_key_id"`
	TrustKeyFile string `yaml:"trust_key_file"`
}
type adapterSettings struct {
	GitHubActions githubActionsSettings `yaml:"github_actions"`
	ECS           ecsSettings           `yaml:"ecs"`
	Datadog       datadogSettings       `yaml:"datadog"`
}
type ecsSettings struct {
	Targets []ecsTargetSettings `yaml:"targets"`
}
type ecsTargetSettings struct {
	Service                  string                      `yaml:"service"`
	Environment              string                      `yaml:"environment"`
	Region                   string                      `yaml:"region"`
	ClusterARN               string                      `yaml:"cluster_arn"`
	ServiceARN               string                      `yaml:"service_arn"`
	VerifyRoleARN            string                      `yaml:"verify_role_arn"`
	DeployRoleARN            string                      `yaml:"deploy_role_arn"`
	RollbackRoleARN          string                      `yaml:"rollback_role_arn"`
	ExternalID               string                      `yaml:"external_id"`
	RoleDuration             string                      `yaml:"role_duration"`
	RollbackContainerDigests map[string]string           `yaml:"rollback_container_digests"`
	RollbackTaskDefinition   string                      `yaml:"rollback_task_definition"`
	TaskDefinitions          []ecsTaskDefinitionSettings `yaml:"task_definitions"`
}
type ecsTaskDefinitionSettings struct {
	ArtifactDigest           string            `yaml:"artifact_digest"`
	TaskDefinitionARN        string            `yaml:"task_definition_arn"`
	ExpectedContainerDigests map[string]string `yaml:"expected_container_digests"`
	ExpectedTaskDefinitions  []string          `yaml:"expected_task_definitions"`
}
type githubActionsSettings struct {
	Targets []githubTargetSettings `yaml:"targets"`
}
type githubTargetSettings struct {
	Service          string `yaml:"service"`
	Environment      string `yaml:"environment"`
	Owner            string `yaml:"owner"`
	Repository       string `yaml:"repository"`
	DeployWorkflow   string `yaml:"deploy_workflow"`
	RollbackWorkflow string `yaml:"rollback_workflow"`
	Ref              string `yaml:"ref"`
}
type datadogSettings struct {
	Site    datadog.Site           `yaml:"site"`
	Queries []datadogQuerySettings `yaml:"queries"`
}
type datadogQuerySettings struct {
	Service       string  `yaml:"service"`
	Environment   string  `yaml:"environment"`
	Expression    string  `yaml:"expression"`
	Comparator    string  `yaml:"comparator"`
	Threshold     float64 `yaml:"threshold"`
	Aggregation   string  `yaml:"aggregation"`
	MinimumPoints int     `yaml:"minimum_points"`
	MaximumAge    string  `yaml:"maximum_age"`
}
type credentialSettings struct {
	GitHubTokenFile       string `yaml:"github_token_file"`
	DatadogCredentialFile string `yaml:"datadog_credential_file"`
}

func main() {
	configPath := flag.String("config", "config/runner.example.yaml", "runner configuration path")
	check := flag.Bool("check-config", false, "validate configuration and exit")
	migrateJournal := flag.Bool("migrate-journal", false, "apply Runner-only migrations and exit (migration DB role required)")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	configuration, err := load(*configPath)
	if err != nil {
		logger.Error("load runner config", "error", err)
		os.Exit(1)
	}
	if *check {
		logger.Info("runner configuration valid", "runner_id", configuration.RunnerID, "runner_group", configuration.RunnerGroup)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *migrateJournal {
		if err := postgresstore.MigrateRunner(ctx, configuration.Journal.DatabaseURL); err != nil {
			logger.Error("migrate runner journal", "error", err)
			os.Exit(1)
		}
		return
	}
	executionRunner, client, reconciler, cleanup, err := buildRunner(ctx, configuration)
	if err != nil {
		logger.Error("initialize runner dependencies", "error", err)
		os.Exit(1)
	}
	defer cleanup()
	server := &http.Server{Addr: configuration.HealthAddress, Handler: healthHandler(configuration, executionRunner.Connectivity, client.Runtime), ReadHeaderTimeout: 2 * time.Second}
	go func() {
		logger.Info("runner health endpoint listening", "address", configuration.HealthAddress, "runner_group", configuration.RunnerGroup)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("runner health server", "error", err)
		}
	}()
	if reconciler != nil {
		go reconcileLoop(ctx, logger, executionRunner, reconciler)
	}
	logger.Info("runner outbound transport starting", "control_plane", configuration.ControlPlane.Address, "runner_id", configuration.RunnerID)
	if err := client.Run(ctx, executionRunner); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("runner outbound transport stopped", "error", err)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
}

func load(path string) (settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return settings{}, err
	}
	data = []byte(os.ExpandEnv(string(data)))
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	var result settings
	if err := decoder.Decode(&result); err != nil {
		return settings{}, err
	}
	if result.Mode != "development" && result.Mode != "production" {
		return settings{}, fmt.Errorf("mode must be development or production")
	}
	if result.RunnerID == "" || result.RunnerGroup == "" || result.TenantID == "" || result.ControlPlane.Address == "" || result.ControlPlane.Issuer == "" || result.Identity.Issuer == "" || result.Identity.Audience == "" || len(result.Capabilities) == 0 {
		return settings{}, fmt.Errorf("runner identity, trust endpoints, policy bundle, and capabilities are required")
	}
	if result.HealthAddress == "" {
		result.HealthAddress = ":8081"
	}
	if result.Capacity == 0 {
		result.Capacity = 1
	}
	if result.Capacity < 1 {
		return settings{}, fmt.Errorf("capacity must be at least one")
	}
	for _, capability := range result.Capabilities {
		if capability != protocol.CapabilityVerify && capability != protocol.CapabilityDeploy && capability != protocol.CapabilityRollback {
			return settings{}, fmt.Errorf("unsupported capability %q", capability)
		}
	}
	if result.Context.Mode == "" {
		result.Context.Mode = "synthetic"
	}
	if err := result.Context.Validate(); err != nil {
		return settings{}, err
	}

	if result.Mode == "production" && (result.Journal.DatabaseURL == "" || result.ControlPlane.ClientCertFile == "" || result.ControlPlane.ClientKeyFile == "" || result.ControlPlane.CAFile == "" || result.Identity.DiscoveryURL == "" || result.Identity.TrustKeyID != "" || result.Identity.TrustKeyFile != "" || (result.Identity.AttestationIssuer == "" || result.Identity.AttestationTrustBundleFile == "" || result.Identity.AttestationIssuer == result.ControlPlane.Issuer || result.Identity.DirectoryFile == "" || result.Identity.DirectoryIssuer == "" || result.Identity.DirectoryTrustBundleFile == "" || result.Identity.DirectoryMinimumRevision < 1 || result.Identity.DelegationsFile != "") || (result.Policy.Issuer == "" || result.Policy.TrustBundleFile == "" || result.Policy.Issuer == result.ControlPlane.Issuer || result.Policy.BundleFile != "" || result.Policy.TrustKeyID != "" || result.Policy.TrustKeyFile != "")) {
		return settings{}, fmt.Errorf("production requires durable journal and customer-managed OIDC and policy trust keys")
	}
	if result.Mode == "production" && (result.Context.Mode != "signed" || result.Context.Issuer == result.ControlPlane.Issuer || result.Context.BuildIssuer == result.ControlPlane.Issuer) {
		return settings{}, fmt.Errorf("production requires independent signed source Context")
	}
	if err := validateIdentityTrust(result.Identity, result.Mode); err != nil {
		return settings{}, err
	}
	if result.Mode == "production" && !strings.HasPrefix(result.ControlPlane.Address, "https://") {
		return settings{}, fmt.Errorf("production control plane address must use https")
	}
	if result.Mode == "production" && (result.Manifests.File == "" || result.Manifests.Issuer == "" || result.Manifests.TrustKeyID == "" || result.Manifests.TrustKeyFile == "") {
		return settings{}, errors.New("production requires independently signed plan manifests and manifest trust key")
	}
	if err := validateAdapters(result); err != nil {
		return settings{}, err
	}
	if err := validateGrantTrust(result.Mode, result.ControlPlane); err != nil {
		return settings{}, err
	}
	if err := validateResultSigning(result.Mode, result.ResultSigning); err != nil {
		return settings{}, err
	}
	return result, nil
}

func validateGrantTrust(mode string, configuration controlPlaneSettings) error {
	hasBundle := configuration.TrustBundleFile != ""
	if configuration.TrustBundleHash != "" && !hasBundle {
		return errors.New("Grant trust bundle hash requires a bundle file")
	}
	hasLegacy := configuration.TrustKeyID != "" || configuration.TrustKeyFile != ""
	if hasBundle && hasLegacy {
		return errors.New("configure either the Grant trust bundle or the legacy development key, not both")
	}
	if hasLegacy && (configuration.TrustKeyID == "" || configuration.TrustKeyFile == "") {
		return errors.New("legacy Grant trust key ID and file must be configured together")
	}
	if mode == "production" && !hasBundle {
		return errors.New("production requires a refreshable Grant trust bundle")
	}
	if mode == "development" && !hasBundle && !hasLegacy {
		return errors.New("development requires a Grant trust bundle or explicit development trust key")
	}
	return nil
}

func validateIdentityTrust(configuration identitySettings, mode string) error {
	staticConfigured := configuration.TrustKeyID != "" || configuration.TrustKeyFile != ""
	if staticConfigured && (configuration.TrustKeyID == "" || configuration.TrustKeyFile == "") {
		return errors.New("static OIDC trust key ID and file must be configured together")
	}
	if configuration.DiscoveryURL != "" && staticConfigured {
		return errors.New("configure OIDC Discovery/JWKS or a static development key, not both")
	}
	if configuration.DiscoveryURL != "" {
		if !strings.HasPrefix(configuration.DiscoveryURL, "https://") {
			return errors.New("OIDC Discovery URL must use https")
		}
		if configuration.CacheTTL != "" {
			ttl, err := time.ParseDuration(configuration.CacheTTL)
			if err != nil || ttl < time.Second || ttl > time.Hour {
				return errors.New("OIDC JWKS cache TTL must be between 1s and 1h")
			}
		}
		return nil
	}
	if mode == "production" {
		return errors.New("production requires trusted OIDC Discovery/JWKS configuration")
	}
	if !staticConfigured {
		return errors.New("development requires OIDC Discovery/JWKS or an explicit static trust key")
	}
	return nil
}

func loadOIDCKeyResolver(configuration identitySettings, mode string) (identity.OIDCKeyResolver, error) {
	if configuration.DiscoveryURL != "" {
		ttl := time.Duration(0)
		if configuration.CacheTTL != "" {
			parsed, err := time.ParseDuration(configuration.CacheTTL)
			if err != nil {
				return nil, err
			}
			ttl = parsed
		}
		return identity.NewOIDCJWKSResolver(map[string]identity.OIDCIssuerConfig{
			configuration.Issuer: {DiscoveryURL: configuration.DiscoveryURL, CacheTTL: ttl},
		})
	}
	if mode != "development" {
		return nil, errors.New("static OIDC trust keys are development-only")
	}
	if configuration.TrustKeyID == "" || configuration.TrustKeyFile == "" {
		return nil, errors.New("development OIDC trust key ID and file are required")
	}
	key, err := grant.LoadEd25519PublicKey(configuration.TrustKeyFile)
	if err != nil {
		return nil, err
	}
	return identity.StaticOIDCKeys{configuration.Issuer + "\x00" + configuration.TrustKeyID: key}, nil
}

func loadGrantTrustKeys(ctx context.Context, configuration settings) (grant.KeyResolver, error) {
	if configuration.ControlPlane.TrustBundleFile != "" {
		keys := grant.FileKeys{ExpectedHash: configuration.ControlPlane.TrustBundleHash, Path: configuration.ControlPlane.TrustBundleFile, Issuer: configuration.ControlPlane.Issuer, Tenant: configuration.TenantID}
		if err := keys.Validate(ctx); err != nil {
			return nil, err
		}
		return keys, nil
	}
	if configuration.Mode != "development" {
		return nil, errors.New("legacy Grant trust keys are development-only")
	}
	key, err := grant.LoadEd25519PublicKey(configuration.ControlPlane.TrustKeyFile)
	if err != nil {
		return nil, err
	}
	return grant.StaticKeys{configuration.ControlPlane.Issuer + "\x00" + configuration.ControlPlane.TrustKeyID: key}, nil
}

func validateAdapters(configuration settings) error {
	githubTargets := make(map[githubactions.TargetKey]githubactions.Target, len(configuration.Adapters.GitHubActions.Targets))
	for _, target := range configuration.Adapters.GitHubActions.Targets {
		key := githubactions.TargetKey{Service: target.Service, Environment: target.Environment}
		if _, duplicate := githubTargets[key]; duplicate {
			return fmt.Errorf("duplicate GitHub Actions target %s/%s", key.Service, key.Environment)
		}
		githubTargets[key] = githubactions.Target{Owner: target.Owner, Repository: target.Repository, DeployWorkflow: target.DeployWorkflow, RollbackWorkflow: target.RollbackWorkflow, Ref: target.Ref}
	}
	if len(githubTargets) > 0 {
		if _, err := githubactions.New(githubactions.Config{Targets: githubTargets}, nil); err != nil {
			return fmt.Errorf("GitHub Actions adapter: %w", err)
		}
	}
	ecsTargets, _, err := buildECSTargets(configuration)
	if err != nil {
		return err
	}
	if len(ecsTargets) > 0 {
		if err := (ecsadapter.Config{Targets: ecsTargets}).Validate(); err != nil {
			return fmt.Errorf("ECS adapter: %w", err)
		}
	}
	datadogQueries := make(map[datadog.QueryKey]datadog.Query, len(configuration.Adapters.Datadog.Queries))
	for _, configured := range configuration.Adapters.Datadog.Queries {
		maximumAge, err := time.ParseDuration(configured.MaximumAge)
		if err != nil {
			return fmt.Errorf("Datadog maximum_age for %s/%s: %w", configured.Service, configured.Environment, err)
		}
		key := datadog.QueryKey{Service: configured.Service, Environment: configured.Environment}
		if _, duplicate := datadogQueries[key]; duplicate {
			return fmt.Errorf("duplicate Datadog query %s/%s", key.Service, key.Environment)
		}
		datadogQueries[key] = datadog.Query{Expression: configured.Expression, Comparator: configured.Comparator, Threshold: configured.Threshold, Aggregation: configured.Aggregation, MinimumPoints: configured.MinimumPoints, MaximumAge: maximumAge}
	}
	if len(datadogQueries) > 0 {
		if _, err := datadog.New(datadog.Config{Site: configuration.Adapters.Datadog.Site, Queries: datadogQueries}, nil); err != nil {
			return fmt.Errorf("Datadog adapter: %w", err)
		}
	}
	if configuration.Mode == "production" {
		if len(ecsTargets) == 0 && (len(githubTargets) == 0 || configuration.Credentials.GitHubTokenFile == "") {
			return fmt.Errorf("production deploy requires allow-listed ECS targets or GitHub Actions targets with a Runner-local token file")
		}
		if len(datadogQueries) == 0 || configuration.Credentials.DatadogCredentialFile == "" {
			return fmt.Errorf("production verification requires allow-listed Datadog queries and a Runner-local credential file")
		}
	}
	return nil
}

func buildRunner(ctx context.Context, configuration settings) (*runnercore.Runner, *runnertransport.RunnerClient, runnercore.Reconciler, func(), error) {
	reportSigner, err := loadResultSigner(ctx, configuration)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	grantKeys, err := loadGrantTrustKeys(ctx, configuration)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load Control Plane grant trust: %w", err)
	}
	oidcKeys, err := loadOIDCKeyResolver(configuration.Identity, configuration.Mode)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load OIDC trust: %w", err)
	}
	var delegationResolver identity.DelegationResolver
	if configuration.Mode != "production" {
		loaded, err := loadDelegations(configuration.Identity.DelegationsFile)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		delegationResolver = loaded
	}
	policyEngine, err := policy.NewMandatoryEngine(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	journal, cleanup, err := openJournal(ctx, configuration)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	fail := func(err error) (*runnercore.Runner, *runnertransport.RunnerClient, runnercore.Reconciler, func(), error) {
		cleanup()
		return nil, nil, nil, nil, err
	}
	subjectVerifier := runnercore.SubjectVerifier(&identity.OIDCVerifier{Issuer: configuration.Identity.Issuer, Audience: configuration.Identity.Audience, Keys: oidcKeys, ClockSkew: 30 * time.Second})
	delegationVerifier := &identity.DelegationVerifier{Resolver: delegationResolver}
	if configuration.Mode == "production" {
		ledger, ok := journal.(identity.DirectoryLedger)
		if !ok {
			return fail(errors.New("production journal requires durable identity Directory ledger"))
		}
		cfg := configuration.Identity
		directory := &identity.FileDirectory{Path: cfg.DirectoryFile, Issuer: cfg.DirectoryIssuer, TenantID: configuration.TenantID, MinimumRevision: cfg.DirectoryMinimumRevision, Keys: grant.FileKeys{Path: cfg.DirectoryTrustBundleFile, Issuer: cfg.DirectoryIssuer, Tenant: configuration.TenantID}, Ledger: ledger}
		if _, err := directory.Read(ctx); err != nil {
			return fail(err)
		}
		subjectVerifier = &identity.AttestationVerifier{Issuer: cfg.AttestationIssuer, TenantID: configuration.TenantID, Audience: cfg.Audience, Keys: grant.FileKeys{Path: cfg.AttestationTrustBundleFile, Issuer: cfg.AttestationIssuer, Tenant: configuration.TenantID}, OIDC: &identity.OIDCVerifier{Issuer: cfg.Issuer, Audience: cfg.Audience, Keys: oidcKeys}, Directory: directory}
		delegationVerifier = &identity.DelegationVerifier{TenantID: configuration.TenantID, TenantClaim: "tenant_id", Resolver: directory}
	}
	deployAdapter, broker, err := buildDeployAdapter(ctx, configuration)
	if err != nil {
		return fail(err)
	}
	observations, err := buildObservations(ctx, configuration, deployAdapter, broker)
	if err != nil {
		return fail(err)
	}
	dispatcher := &runnercore.SDKDispatcher{Adapter: deployAdapter}
	grantVerifier := &grant.Verifier{Issuer: configuration.ControlPlane.Issuer, RunnerGroup: configuration.RunnerGroup, TenantID: configuration.TenantID, Keys: grantKeys, ClockSkew: 30 * time.Second}
	manifestStore, ok := journal.(runnercore.ManifestStore)
	if !ok {
		return fail(errors.New("journal does not support durable manifests"))
	}
	received := &runnercore.ManifestContexts{Store: manifestStore, TenantID: configuration.TenantID, RunnerGroup: configuration.RunnerGroup, Issuer: configuration.Manifests.Issuer}
	if configuration.Manifests.TrustKeyFile != "" {
		key, err := grant.LoadEd25519PublicKey(configuration.Manifests.TrustKeyFile)
		if err != nil {
			return fail(err)
		}
		received.Keys = grant.StaticKeys{configuration.Manifests.Issuer + "\x00" + configuration.Manifests.TrustKeyID: key}
	}
	connectivity := &runnercore.AtomicConnectionState{}
	capacity := configuration.Capacity
	if capacity < 1 {
		capacity = 1
	}
	runtimeState := runnercore.NewRuntimeState(capacity)
	executionRunner := &runnercore.Runner{
		RunnerID: configuration.RunnerID,
		TenantID: configuration.TenantID, RunnerGroup: configuration.RunnerGroup, Grants: grantVerifier,
		Subjects:    subjectVerifier,
		Delegations: delegationVerifier, Contexts: received, Approvals: received,
		Capabilities: runnercore.NewCapabilitySet(configuration.Capabilities...), Policy: policyEngine, Journal: journal,
		Observations: observations, Credentials: broker, Adapter: dispatcher, Connectivity: connectivity, Admission: runtimeState,
	}
	httpClient, token, err := buildRunnerTransportClient(configuration)
	if err != nil {
		return fail(err)
	}
	client := &runnertransport.RunnerClient{
		ResultSigner: reportSigner, TenantID: configuration.TenantID, RunnerGroup: configuration.RunnerGroup,
		BaseURL: configuration.ControlPlane.Address, RunnerID: configuration.RunnerID, Token: token,
		HTTP: httpClient, Wait: 30 * time.Second, Connectivity: connectivity, Runtime: runtimeState, Capacity: capacity,
		BeforeExecute: func(ctx context.Context, actionGrant protocol.ActionGrant) error {
			if err := grantVerifier.Verify(ctx, actionGrant); err != nil {
				return err
			}
			if actionGrant.Action.Capability == protocol.CapabilityVerify {
				return nil
			}
			return received.LoadFile(ctx, configuration.Manifests.File)
		},
	}
	if configuration.Mode == "production" {
		ledger, ok := journal.(policy.RevisionLedger)
		if !ok {
			return fail(errors.New("production Policy requires durable revision fence"))
		}
		keys := policy.FileBundleKeys{Trust: grant.FileKeys{Path: configuration.Policy.TrustBundleFile, Issuer: configuration.Policy.Issuer, Tenant: configuration.TenantID}}
		source := &policy.RunnerPolicySource{TenantID: configuration.TenantID, RunnerGroup: configuration.RunnerGroup, Issuer: configuration.Policy.Issuer, Keys: keys, Ledger: ledger, Fetch: client.FetchPolicy, Acknowledge: client.AcknowledgePolicy}
		executionRunner.Policy = policy.NewEngine(&policy.AssignedEvaluator{Load: source.Load, Issuer: source.Issuer, Keys: keys})
	}
	if configuration.Context.Mode == "signed" {
		ledger, ok := journal.(contextsnapshot.ContextLedger)
		if !ok {
			return fail(errors.New("signed Context requires durable source revision fence"))
		}
		sink, ok := journal.(contextsnapshot.CIAuditSink)
		if !ok {
			return fail(errors.New("signed Context requires durable source audit"))
		}
		trust, err := configuration.Context.Trust(configuration.TenantID)
		if err != nil {
			return fail(err)
		}
		executionRunner.Sources = &application.SourceAuthority{Source: &contextsnapshot.FileAdmissionSource{Path: configuration.Context.File, Trust: trust, Ledger: ledger, Audit: sink}}
	}
	executionRunner.Reporter = client
	return executionRunner, client, dispatcher, cleanup, nil
}

func buildRunnerTransportClient(configuration settings) (*http.Client, string, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	configured := configuration.ControlPlane
	if configured.ClientCertFile != "" || configured.ClientKeyFile != "" || configured.CAFile != "" {
		if configured.ClientCertFile == "" || configured.ClientKeyFile == "" || configured.CAFile == "" {
			return nil, "", errors.New("Runner mTLS requires client certificate, client key, and Control Plane CA files")
		}
		keyInfo, err := os.Stat(configured.ClientKeyFile)
		if err != nil {
			return nil, "", fmt.Errorf("stat Runner workload key: %w", err)
		}
		if keyInfo.Mode().Perm()&0o077 != 0 {
			return nil, "", errors.New("Runner workload key must not be group or world accessible")
		}
		certificate, err := tls.LoadX509KeyPair(configured.ClientCertFile, configured.ClientKeyFile)
		if err != nil {
			return nil, "", fmt.Errorf("load Runner workload certificate: %w", err)
		}
		data, err := os.ReadFile(configured.CAFile)
		if err != nil {
			return nil, "", fmt.Errorf("read Control Plane CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, "", errors.New("Control Plane CA contains no certificates")
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{certificate}
		transport.TLSClientConfig.RootCAs = roots
		return &http.Client{Transport: transport, Timeout: 40 * time.Second}, "", nil
	}
	if configured.TokenFile == "" {
		return nil, "", errors.New("Runner transport requires mTLS workload identity or a development token")
	}
	token, err := readSecret(configured.TokenFile)
	if err != nil {
		return nil, "", fmt.Errorf("read Control Plane runner token: %w", err)
	}
	return &http.Client{Transport: transport, Timeout: 40 * time.Second}, token, nil
}

func buildDeployAdapter(ctx context.Context, configuration settings) (adapter.DeployAdapter, credentials.Broker, error) {
	ecsTargets, roles, err := buildECSTargets(configuration)
	if err != nil {
		return nil, nil, err
	}
	if len(ecsTargets) > 0 {
		awsConfiguration, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("load Runner AWS workload identity: %w", err)
		}
		instance, err := ecsadapter.New(ecsadapter.Config{Targets: ecsTargets}, ecsadapter.AWSClientFactory(awsConfiguration))
		if err != nil {
			return nil, nil, err
		}
		return instance, &credentials.AWSBroker{Client: sts.NewFromConfig(awsConfiguration), Roles: roles}, nil
	}
	targets := make(map[githubactions.TargetKey]githubactions.Target, len(configuration.Adapters.GitHubActions.Targets))
	allowed := make(map[credentials.GitHubTarget]struct{}, len(configuration.Adapters.GitHubActions.Targets))
	for _, target := range configuration.Adapters.GitHubActions.Targets {
		targets[githubactions.TargetKey{Service: target.Service, Environment: target.Environment}] = githubactions.Target{Owner: target.Owner, Repository: target.Repository, DeployWorkflow: target.DeployWorkflow, RollbackWorkflow: target.RollbackWorkflow, Ref: target.Ref}
		allowed[credentials.GitHubTarget{TenantID: configuration.TenantID, Service: target.Service, Environment: target.Environment}] = struct{}{}
	}
	instance, err := githubactions.New(githubactions.Config{Targets: targets}, nil)
	if err != nil {
		return nil, nil, err
	}
	broker := &credentials.GitHubBroker{Source: credentials.GitHubTokenFileSource{Path: configuration.Credentials.GitHubTokenFile}, Allowed: allowed}
	return instance, broker, nil
}

func buildECSTargets(configuration settings) (map[ecsadapter.TargetKey]ecsadapter.Target, map[credentials.AWSRoleKey]credentials.AWSRole, error) {
	targets := make(map[ecsadapter.TargetKey]ecsadapter.Target, len(configuration.Adapters.ECS.Targets))
	roles := make(map[credentials.AWSRoleKey]credentials.AWSRole, len(configuration.Adapters.ECS.Targets)*2)
	for _, configured := range configuration.Adapters.ECS.Targets {
		key := ecsadapter.TargetKey{Service: configured.Service, Environment: configured.Environment}
		if configured.DeployRoleARN == "" {
			return nil, nil, fmt.Errorf("ECS deploy role ARN for %s/%s is required", key.Service, key.Environment)
		}
		if configured.RollbackTaskDefinition != "" && configured.RollbackRoleARN == "" {
			return nil, nil, fmt.Errorf("ECS rollback role ARN for %s/%s is required when rollback is enabled", key.Service, key.Environment)
		}
		if configured.RollbackRoleARN != "" && configured.RollbackRoleARN == configured.DeployRoleARN {
			return nil, nil, fmt.Errorf("ECS deploy and rollback roles for %s/%s must be distinct", key.Service, key.Environment)
		}
		if configured.RollbackTaskDefinition == "" && configured.RollbackRoleARN != "" {
			return nil, nil, fmt.Errorf("ECS rollback role ARN for %s/%s requires a rollback task definition", key.Service, key.Environment)
		}
		if _, duplicate := targets[key]; duplicate {
			return nil, nil, fmt.Errorf("duplicate ECS target %s/%s", key.Service, key.Environment)
		}
		duration := 15 * time.Minute
		if configured.RoleDuration != "" {
			parsed, err := time.ParseDuration(configured.RoleDuration)
			if err != nil {
				return nil, nil, fmt.Errorf("ECS role duration for %s/%s: %w", key.Service, key.Environment, err)
			}
			duration = parsed
		}
		if duration < 15*time.Minute || duration > 12*time.Hour {
			return nil, nil, fmt.Errorf("ECS role duration for %s/%s must be between 15m and 12h", key.Service, key.Environment)
		}
		definitions := make(map[string]ecsadapter.TaskDefinition, len(configured.TaskDefinitions))
		for _, definition := range configured.TaskDefinitions {
			if _, duplicate := definitions[definition.ArtifactDigest]; duplicate {
				return nil, nil, fmt.Errorf("duplicate ECS artifact digest for %s/%s", key.Service, key.Environment)
			}
			definitions[definition.ArtifactDigest] = ecsadapter.TaskDefinition{ARN: definition.TaskDefinitionARN, ExpectedContainerDigests: definition.ExpectedContainerDigests, ExpectedTaskDefinitions: append([]string(nil), definition.ExpectedTaskDefinitions...)}
		}
		targets[key] = ecsadapter.Target{Region: configured.Region, ClusterARN: configured.ClusterARN, ServiceARN: configured.ServiceARN, TaskDefinitions: definitions, RollbackTaskDefinition: configured.RollbackTaskDefinition, RollbackContainerDigests: configured.RollbackContainerDigests}
		roles[credentials.AWSRoleKey{TenantID: configuration.TenantID, Service: key.Service, Environment: key.Environment, Purpose: credentials.PurposeDeploy}] = credentials.AWSRole{RoleARN: configured.DeployRoleARN, ExternalID: configured.ExternalID, Duration: duration}
		if configured.VerifyRoleARN != "" {
			if configured.VerifyRoleARN == configured.DeployRoleARN || configured.VerifyRoleARN == configured.RollbackRoleARN {
				return nil, nil, fmt.Errorf("ECS verify role must be distinct from write roles")
			}
			roles[credentials.AWSRoleKey{TenantID: configuration.TenantID, Service: key.Service, Environment: key.Environment, Purpose: credentials.PurposeVerify}] = credentials.AWSRole{RoleARN: configured.VerifyRoleARN, ExternalID: configured.ExternalID, Duration: duration}
		}
		if configured.RollbackRoleARN != "" {
			roles[credentials.AWSRoleKey{TenantID: configuration.TenantID, Service: key.Service, Environment: key.Environment, Purpose: credentials.PurposeRollback}] = credentials.AWSRole{RoleARN: configured.RollbackRoleARN, ExternalID: configured.ExternalID, Duration: duration}
		}
	}
	return targets, roles, nil
}

func openJournal(ctx context.Context, configuration settings) (store.RunnerJournal, func(), error) {
	if configuration.Journal.DatabaseURL == "" {
		if configuration.Mode != "development" {
			return nil, nil, errors.New("production requires a shared PostgreSQL journal")
		}
		return store.NewMemory(), func() {}, nil
	}
	persistent, err := postgresstore.NewRunnerJournal(ctx, configuration.Journal.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	return persistent, persistent.Close, nil
}

func loadDelegations(path string) (identity.StaticDelegations, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open delegation file: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var values []identity.Delegation
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode delegation file: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode delegation file: %w", err)
	}
	result := make(identity.StaticDelegations, len(values))
	for _, value := range values {
		if value.ID == "" {
			return nil, errors.New("delegation ID is required")
		}
		result[value.ID] = value
	}
	return result, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected data after JSON value")
		}
		return err
	}
	return nil
}

func readSecret(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("secret file must not be group or world accessible")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("secret file is empty")
	}
	return value, nil
}

func reconcileLoop(ctx context.Context, logger *slog.Logger, executionRunner *runnercore.Runner, reconciler runnercore.Reconciler) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if err := executionRunner.Reconcile(ctx, reconciler, 25); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("runner reconciliation", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func healthHandler(configuration settings, connectivity runnercore.Connectivity, runtimeState *runnercore.RuntimeState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		accepting := connectivity != nil && connectivity.Connected() && runtimeState != nil && runtimeState.Accepting()
		capacity, inFlight := runtimeState.Snapshot()
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"status":"alive","accepting_actions":%t,"runner_id":%q,"runner_group":%q,"mode":%q,"capacity":%d,"in_flight":%d}`, accepting, configuration.RunnerID, configuration.RunnerGroup, runtimeState.Mode(), capacity, inFlight)
	})
	return mux
}

func loadResultSigner(ctx context.Context, configuration settings) (*resultreport.Signer, error) {
	if err := validateResultSigning(configuration.Mode, configuration.ResultSigning); err != nil {
		return nil, err
	}
	s := configuration.ResultSigning
	if s.KeyID == "" {
		return nil, errors.New("result_signing.key_id is required")
	}
	var signer grant.Signer
	switch s.Provider {
	case "development":
		if configuration.Mode != "development" {
			return nil, errors.New("development result signer requires development mode")
		}
		loaded, _, err := grant.LoadDevelopmentSigner(s.PrivateKeyFile, s.KeyID)
		if err != nil {
			return nil, err
		}
		signer = loaded
	case "aws-kms":
		if s.AWSRegion == "" {
			return nil, errors.New("result signing aws_region is required")
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(s.AWSRegion))
		if err != nil {
			return nil, err
		}
		loaded, err := grant.NewAWSKMSSigner(kms.NewFromConfig(cfg), s.KeyID)
		if err != nil {
			return nil, err
		}
		signer = loaded
	default:
		return nil, errors.New("result signing provider must be development or aws-kms")
	}
	return &resultreport.Signer{Signer: signer, KeyID: s.KeyID}, nil
}

func validateResultSigning(mode string, s config.GrantSigning) error {
	if s.KeyID == "" {
		return errors.New("result_signing.key_id is required")
	}
	switch s.Provider {
	case "development":
		if mode != "development" {
			return errors.New("development result signer requires development mode")
		}
		if s.PrivateKeyFile == "" {
			return errors.New("development result signing requires private_key_file")
		}
	case "aws-kms":
		if s.AWSRegion == "" {
			return errors.New("result signing aws_region is required")
		}
	default:
		return errors.New("result signing provider must be development or aws-kms")
	}
	return nil
}
