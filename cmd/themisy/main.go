package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"themisy/internal/api"
	"themisy/internal/application"
	"themisy/internal/catalog"
	"themisy/internal/config"
	"themisy/internal/contract"
	"themisy/internal/executor"
	"themisy/internal/grant"
	"themisy/internal/mcp"
	"themisy/internal/planner"
	"themisy/internal/policy"
	"themisy/internal/profile"
	"themisy/internal/scheduler"
	postgresstore "themisy/internal/store/postgres"
	runnertransport "themisy/internal/transport"
	"themisy/internal/ui"
	workflowcore "themisy/internal/workflow"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"go.temporal.io/sdk/client"
)

func main() {
	configPath := flag.String("config", "config/themisy.example.yaml", "Themisy configuration path")
	modeOverride := flag.String("mode", "", "control, worker, or all")
	contractOverride := flag.String("contracts", "", "service contract directory override")
	profileOverride := flag.String("profiles", "", "release profile directory override")
	catalogOverride := flag.String("catalog", "", "legacy JSON service catalog compatibility override")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	settings, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	if *modeOverride != "" {
		settings.Mode = *modeOverride
	}
	if *contractOverride != "" {
		settings.Planning.Contracts = *contractOverride
	}
	if *profileOverride != "" {
		settings.Planning.Profiles = *profileOverride
	}
	if *catalogOverride != "" {
		settings.Planning.LegacyCatalog = *catalogOverride
	}
	if err := settings.Validate(); err != nil {
		logger.Error("validate config", "error", err)
		os.Exit(1)
	}
	ctx := context.Background()
	if settings.Database.AutoMigrate {
		if err := postgresstore.Migrate(settings.Database.URL, false); err != nil {
			logger.Error("migrate postgres", "error", err)
			os.Exit(1)
		}
	}
	persistentStore, err := postgresstore.New(ctx, settings.Database.URL)
	if err != nil {
		logger.Error("open postgres", "error", err)
		os.Exit(1)
	}
	defer persistentStore.Close()
	temporalClient, err := client.Dial(client.Options{HostPort: settings.Temporal.Address, Namespace: settings.Temporal.Namespace})
	if err != nil {
		logger.Error("connect temporal", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()
	contracts, profiles, err := loadPlanningInputs(settings.Planning.Contracts, settings.Planning.Profiles, settings.Planning.LegacyCatalog)
	if err != nil {
		logger.Error("load planning inputs", "error", err)
		os.Exit(1)
	}
	policyEngine, err := policy.NewMandatoryEngine(ctx)
	if err != nil {
		logger.Error("initialize mandatory OPA policy", "error", err)
		os.Exit(1)
	}
	releasePlanner, err := planner.NewFromContracts(contracts, profiles, planner.Options{PolicyHash: policyEngine.PolicyHash(), EvidenceHash: policy.PreDispatchEvidenceHash()})
	if err != nil {
		logger.Error("initialize planner", "error", err)
		os.Exit(1)
	}
	controller := workflowcore.NewController(temporalClient, settings.Temporal.TaskQueue)
	releases := application.NewReleases(releasePlanner, persistentStore, controller)
	go releases.RunWorkflowRecovery(ctx, 5*time.Second, func(err error) { logger.Error("recover workflow outbox", "error", err) })
	grantService, runnerHandler, err := buildGrantPath(ctx, settings, persistentStore)
	if err != nil {
		logger.Error("initialize grant execution path", "error", err)
		os.Exit(1)
	}
	grantExecutor := &application.GrantExecutor{Grants: grantService, Verification: executor.NewMock(nil), PollInterval: 100 * time.Millisecond}
	activities := workflowcore.NewActivities(persistentStore, policyEngine, grantExecutor)
	activities.AdapterName = "runner"
	activities.Capacity = func(input workflowcore.ScheduleInput) scheduler.Capacity {
		return scheduler.Capacity{RunnerAvailable: releases.RunnerCapacity(input.Step.Tenant, input.Step.RunnerGroup), AdapterRemaining: 20, QueueLimit: 100}
	}
	if settings.Mode == "worker" {
		runWorker(logger, temporalClient, settings.Temporal.TaskQueue, activities)
		return
	}
	if settings.Mode == "all" {
		worker := workflowcore.NewWorker(temporalClient, settings.Temporal.TaskQueue, activities)
		if err := worker.Start(); err != nil {
			logger.Error("start temporal worker", "error", err)
			os.Exit(1)
		}
		defer worker.Stop()
	}
	uiServer, err := ui.New(releases, ui.HeaderIdentityVerifier{})
	if err != nil {
		logger.Error("initialize status UI", "error", err)
		os.Exit(1)
	}
	routes := http.NewServeMux()
	routes.Handle("/mcp", mcp.NewHTTP(releases, mcp.HeaderPrincipalResolver{}, nil))
	routes.Handle("/ui/", uiServer.Handler())
	routes.Handle("/v1/runner/", runnerHandler)
	routes.Handle("/", api.New(releases, logger).Handler())
	server := &http.Server{Addr: settings.HTTP.Address, Handler: routes, ReadHeaderTimeout: 5 * time.Second}
	server.TLSConfig, err = buildServerTLS(settings)
	if err != nil {
		logger.Error("initialize server TLS", "error", err)
		os.Exit(1)
	}
	logger.Info("Themisy control plane listening", "address", settings.HTTP.Address, "services", len(contracts), "workflow", "temporal", "store", "postgres")
	serve := server.ListenAndServe
	if settings.HTTP.TLSCertFile != "" {
		serve = func() error { return server.ListenAndServeTLS(settings.HTTP.TLSCertFile, settings.HTTP.TLSKeyFile) }
	}
	if err := serve(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func buildGrantPath(ctx context.Context, settings config.Config, persistentStore *postgresstore.Store) (*application.Grants, http.Handler, error) {
	if settings.Grants.Issuer == "" {
		return nil, nil, errors.New("grants configuration is required")
	}
	var signer grant.Signer
	switch settings.Grants.Signing.Provider {
	case "development":
		loaded, _, err := grant.LoadDevelopmentSigner(settings.Grants.Signing.PrivateKeyFile, settings.Grants.Signing.KeyID)
		if err != nil {
			return nil, nil, err
		}
		signer = loaded
	case "aws-kms":
		awsConfiguration, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(settings.Grants.Signing.AWSRegion))
		if err != nil {
			return nil, nil, fmt.Errorf("load AWS KMS workload identity: %w", err)
		}
		loaded, err := grant.NewAWSKMSSigner(kms.NewFromConfig(awsConfiguration), settings.Grants.Signing.KeyID)
		if err != nil {
			return nil, nil, err
		}
		signer = loaded
	default:
		return nil, nil, fmt.Errorf("unsupported grant signer %q", settings.Grants.Signing.Provider)
	}
	ttl, err := time.ParseDuration(settings.Grants.TTL)
	if err != nil {
		return nil, nil, err
	}
	grants, err := application.NewGrants(persistentStore, signer, settings.Grants.Issuer, ttl)
	if err != nil {
		return nil, nil, err
	}
	if settings.Mode == "worker" {
		return grants, http.NotFoundHandler(), nil
	}
	bearer := runnertransport.StaticRunnerAuthenticator{}
	workloads := runnertransport.MTLSRunnerAuthenticator{}
	for _, registration := range settings.RunnerTransport.Registrations {
		if registration.RunnerID == "" || registration.TenantID == "" || registration.RunnerGroup == "" || (registration.TokenFile == "" && registration.WorkloadIdentity == "") {
			return nil, nil, errors.New("runner registration is incomplete")
		}
		identity := runnertransport.RunnerIdentity{RunnerID: registration.RunnerID, TenantID: registration.TenantID, RunnerGroup: registration.RunnerGroup}
		if registration.TokenFile != "" {
			token, err := readSecretFile(registration.TokenFile)
			if err != nil {
				return nil, nil, fmt.Errorf("runner %s token: %w", registration.RunnerID, err)
			}
			bearer[registration.RunnerID] = runnertransport.RunnerRegistration{TenantID: registration.TenantID, RunnerGroup: registration.RunnerGroup, Token: token}
		}
		if registration.WorkloadIdentity != "" {
			if _, duplicate := workloads[registration.WorkloadIdentity]; duplicate {
				return nil, nil, fmt.Errorf("duplicate runner workload identity %q", registration.WorkloadIdentity)
			}
			workloads[registration.WorkloadIdentity] = identity
		}
	}
	if len(bearer) == 0 && len(workloads) == 0 {
		return nil, nil, errors.New("at least one runner registration is required")
	}
	server := &runnertransport.RunnerServer{Store: persistentStore, Fleet: persistentStore, Audit: persistentStore, Auth: runnertransport.HybridRunnerAuthenticator{MTLS: workloads, Bearer: bearer}, RequireHeartbeat: true}
	return grants, server.Handler(), nil
}

func buildServerTLS(settings config.Config) (*tls.Config, error) {
	if settings.HTTP.TLSCertFile == "" {
		return nil, nil
	}
	keyInfo, err := os.Stat(settings.HTTP.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("stat server TLS key: %w", err)
	}
	if keyInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("server TLS key must not be group or world accessible")
	}
	configuration := &tls.Config{MinVersion: tls.VersionTLS13}
	if settings.RunnerTransport.ClientCAFile == "" {
		return configuration, nil
	}
	data, err := os.ReadFile(settings.RunnerTransport.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read Runner client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("Runner client CA contains no certificates")
	}
	configuration.ClientCAs = pool
	configuration.ClientAuth = tls.VerifyClientCertIfGiven
	return configuration, nil
}

func readSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("secret file must not be group or world accessible")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(value)) == "" {
		return "", errors.New("secret file is empty")
	}
	return strings.TrimSpace(string(value)), nil
}

func runWorker(logger *slog.Logger, temporalClient client.Client, taskQueue string, activities *workflowcore.Activities) {
	logger.Info("temporal worker starting", "task_queue", taskQueue)
	if err := workflowcore.RunWorker(context.Background(), temporalClient, taskQueue, activities); err != nil {
		logger.Error("temporal worker stopped", "error", err)
		os.Exit(1)
	}
}

func loadPlanningInputs(contractPath, profilePath, catalogPath string) ([]contract.ServiceContract, []profile.ReleaseProfile, error) {
	if catalogPath != "" {
		return catalog.LoadContracts(catalogPath)
	}
	contracts, err := contract.LoadDir(contractPath)
	if err != nil {
		return nil, nil, err
	}
	profiles, err := profile.LoadDir(profilePath)
	if err != nil {
		return nil, nil, err
	}
	return contracts, profiles, nil
}
