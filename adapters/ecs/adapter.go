package ecs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"themisy/pkg/adapter"
	appcredentials "themisy/pkg/credentials"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

const (
	Name    = "aws-ecs"
	Version = "v1alpha1"
)

type API interface {
	ListTasks(context.Context, *awsecs.ListTasksInput, ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error)
	DescribeTasks(context.Context, *awsecs.DescribeTasksInput, ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error)
	DescribeServices(context.Context, *awsecs.DescribeServicesInput, ...func(*awsecs.Options)) (*awsecs.DescribeServicesOutput, error)
	UpdateService(context.Context, *awsecs.UpdateServiceInput, ...func(*awsecs.Options)) (*awsecs.UpdateServiceOutput, error)
}

type ClientFactory func(context.Context, string, appcredentials.Credential) (API, error)

type Adapter struct {
	config  Config
	clients ClientFactory
	now     func() time.Time
}

func New(config Config, clients ClientFactory) (*Adapter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if clients == nil {
		return nil, errors.New("ECS client factory is required")
	}
	// Own every mutable level of the operator allowlist. Later edits to the
	// constructor input must not change a live adapter's resource selection.
	owned := Config{Targets: make(map[TargetKey]Target, len(config.Targets))}
	for key, target := range config.Targets {
		definitions := make(map[string]TaskDefinition, len(target.TaskDefinitions))
		for digest, definition := range target.TaskDefinitions {
			definition.ExpectedContainerDigests = maps.Clone(definition.ExpectedContainerDigests)
			definition.ExpectedTaskDefinitions = slices.Clone(definition.ExpectedTaskDefinitions)
			definitions[digest] = definition
		}
		target.TaskDefinitions = definitions
		target.RollbackContainerDigests = maps.Clone(target.RollbackContainerDigests)
		owned.Targets[key] = target
	}
	return &Adapter{config: owned, clients: clients, now: time.Now}, nil
}

func AWSClientFactory(base aws.Config) ClientFactory {
	return func(_ context.Context, region string, credential appcredentials.Credential) (API, error) {
		accessKey := string(credential.Value(appcredentials.AWSAccessKeyID))
		secretKey := string(credential.Value(appcredentials.AWSSecretAccessKey))
		sessionToken := string(credential.Value(appcredentials.AWSSessionToken))
		if accessKey == "" || secretKey == "" || sessionToken == "" || !credential.ValidAt(time.Now()) {
			return nil, errors.New("valid STS credentials are required")
		}
		configured := base.Copy()
		configured.Region = region
		configured.Credentials = aws.NewCredentialsCache(awscredentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken))
		return awsecs.NewFromConfig(configured, func(options *awsecs.Options) { options.RetryMaxAttempts = 1 }), nil
	}
}

func (a *Adapter) Name() string               { return Name }
func (a *Adapter) Version() string            { return Version }
func (a *Adapter) CredentialProvider() string { return adapter.ProviderAWS }

// Status returns a read-only snapshot for an allow-listed logical target.
func (a *Adapter) Status(ctx context.Context, request adapter.StatusRequest, credential appcredentials.Credential) (adapter.DeploymentStatus, error) {
	if request.Target.Service == "" || request.Target.Environment == "" {
		return adapter.DeploymentStatus{}, terminal("status", "", errors.New("target service and environment are required"))
	}
	target, ok := a.config.Targets[TargetKey{Service: request.Target.Service, Environment: request.Target.Environment}]
	if !ok {
		return adapter.DeploymentStatus{}, terminal("status", "", errors.New("logical ECS target is not allow-listed"))
	}
	client, err := a.clients(ctx, target.Region, credential)
	if err != nil {
		return adapter.DeploymentStatus{}, retryable("status", request.Target.Service+"/"+request.Target.Environment, err)
	}
	service, err := describeService(ctx, client, target)
	if err != nil {
		return adapter.DeploymentStatus{}, retryable("describe-services", request.Target.Service+"/"+request.Target.Environment, err)
	}
	status := adapter.DeploymentStatus{
		ServiceStatus:  aws.ToString(service.Status),
		TaskDefinition: aws.ToString(service.TaskDefinition),
		DesiredCount:   service.DesiredCount,
		RunningCount:   service.RunningCount,
		PendingCount:   service.PendingCount,
	}
	for _, deployment := range service.Deployments {
		status.Deployments = append(status.Deployments, adapter.ServiceDeploymentStatus{
			TaskDefinition: aws.ToString(deployment.TaskDefinition),
			Status:         aws.ToString(deployment.Status),
			RolloutState:   string(deployment.RolloutState),
			RunningCount:   deployment.RunningCount,
			DesiredCount:   deployment.DesiredCount,
		})
	}
	return status, nil
}

func (a *Adapter) Authorize(target adapter.Target, artifactDigest string) error {
	_, _, err := a.resolve(target, artifactDigest)
	return err
}

func (a *Adapter) Deploy(ctx context.Context, request adapter.DeployRequest, credential appcredentials.Credential) (adapter.Deployment, error) {
	if err := adapter.ValidateDeployRequest(request); err != nil {
		return adapter.Deployment{}, terminal("deploy", request.IdempotencyKey, err)
	}
	target, definition, err := a.resolve(request.Target, request.ArtifactDigest)
	if err != nil {
		return adapter.Deployment{}, terminal("deploy", request.IdempotencyKey, err)
	}
	client, err := a.clients(ctx, target.Region, credential)
	if err != nil {
		return adapter.Deployment{}, retryable("deploy", request.IdempotencyKey, err)
	}
	current, err := describe(ctx, client, target)
	if err != nil {
		return adapter.Deployment{}, retryable("describe-services", request.IdempotencyKey, err)
	}
	if current == definition.ARN {
		return a.confirmRuntime(ctx, client, target, definition.ARN, definition.ExpectedContainerDigests, request.IdempotencyKey)
	}
	if !slices.Contains(definition.ExpectedTaskDefinitions, current) {
		return adapter.Deployment{}, terminal("expected-task-definition", request.IdempotencyKey, fmt.Errorf("service task definition is not an allowed predecessor"))
	}
	output, err := client.UpdateService(ctx, &awsecs.UpdateServiceInput{Cluster: aws.String(target.ClusterARN), Service: aws.String(target.ServiceARN), TaskDefinition: aws.String(definition.ARN)}, func(options *awsecs.Options) { options.RetryMaxAttempts = 1 })
	if err != nil {
		// Once UpdateService was sent, every error is ambiguous. Retrying here
		// could create a second write; reconciliation must determine reality.
		return adapter.Deployment{}, unknown("update-service", request.IdempotencyKey, err)
	}
	if output == nil || output.Service == nil || aws.ToString(output.Service.TaskDefinition) != definition.ARN {
		return adapter.Deployment{}, unknown("update-service", request.IdempotencyKey, errors.New("ECS response did not confirm the trusted task definition"))
	}
	return a.confirmRuntime(ctx, client, target, definition.ARN, definition.ExpectedContainerDigests, request.IdempotencyKey)
}

func (a *Adapter) Rollback(ctx context.Context, request adapter.RollbackRequest, credential appcredentials.Credential) (adapter.Deployment, error) {
	if err := adapter.ValidateRollbackRequest(request); err != nil {
		return adapter.Deployment{}, terminal("rollback", request.IdempotencyKey, err)
	}
	target, ok := a.config.Targets[TargetKey{Service: request.Target.Service, Environment: request.Target.Environment}]
	if !ok || target.RollbackTaskDefinition == "" {
		return adapter.Deployment{}, terminal("rollback", request.IdempotencyKey, errors.New("trusted rollback task definition is unavailable"))
	}
	client, err := a.clients(ctx, target.Region, credential)
	if err != nil {
		return adapter.Deployment{}, retryable("rollback", request.IdempotencyKey, err)
	}
	current, err := describe(ctx, client, target)
	if err != nil {
		return adapter.Deployment{}, retryable("describe-services", request.IdempotencyKey, err)
	}
	if current == target.RollbackTaskDefinition {
		return a.confirmRuntime(ctx, client, target, target.RollbackTaskDefinition, target.RollbackContainerDigests, request.IdempotencyKey)
	}
	if !configuredTaskDefinition(target, current) {
		return adapter.Deployment{}, terminal("rollback", request.IdempotencyKey, errors.New("current task definition is outside the trusted target map"))
	}
	output, err := client.UpdateService(ctx, &awsecs.UpdateServiceInput{Cluster: aws.String(target.ClusterARN), Service: aws.String(target.ServiceARN), TaskDefinition: aws.String(target.RollbackTaskDefinition)}, func(options *awsecs.Options) { options.RetryMaxAttempts = 1 })
	if err != nil || output == nil || output.Service == nil || aws.ToString(output.Service.TaskDefinition) != target.RollbackTaskDefinition {
		if err == nil {
			err = errors.New("ECS response did not confirm the trusted rollback task definition")
		}
		return adapter.Deployment{}, unknown("update-service-rollback", request.IdempotencyKey, err)
	}
	return a.confirmRuntime(ctx, client, target, target.RollbackTaskDefinition, target.RollbackContainerDigests, request.IdempotencyKey)
}

func (a *Adapter) resolve(target adapter.Target, digest string) (Target, TaskDefinition, error) {
	configured, ok := a.config.Targets[TargetKey{Service: target.Service, Environment: target.Environment}]
	if !ok {
		return Target{}, TaskDefinition{}, errors.New("logical ECS target is not allow-listed")
	}
	definition, ok := configured.TaskDefinitions[digest]
	if !ok {
		return Target{}, TaskDefinition{}, errors.New("artifact digest has no trusted ECS task definition mapping")
	}
	return configured, definition, nil
}

func (a *Adapter) deployment(idempotencyKey string) adapter.Deployment {
	now := a.now().UTC()
	return adapter.Deployment{ExternalExecutionID: "ecs:" + adapter.CorrelationID(idempotencyKey), CorrelationID: adapter.CorrelationID(idempotencyKey), StartedAt: now, FinishedAt: now}
}

func describe(ctx context.Context, client API, target Target) (string, error) {
	service, err := describeService(ctx, client, target)
	if err != nil {
		return "", err
	}
	return aws.ToString(service.TaskDefinition), nil
}

func describeService(ctx context.Context, client API, target Target) (types.Service, error) {
	output, err := client.DescribeServices(ctx, &awsecs.DescribeServicesInput{Cluster: aws.String(target.ClusterARN), Services: []string{target.ServiceARN}})
	if err != nil {
		return types.Service{}, err
	}
	if output == nil || len(output.Failures) != 0 || len(output.Services) != 1 || output.Services[0].TaskDefinition == nil || aws.ToString(output.Services[0].ServiceArn) != target.ServiceARN || aws.ToString(output.Services[0].ClusterArn) != target.ClusterARN {
		return types.Service{}, errors.New("ECS DescribeServices returned an ambiguous service set")
	}
	return output.Services[0], nil
}

func configuredTaskDefinition(target Target, value string) bool {
	for _, definition := range target.TaskDefinitions {
		if definition.ARN == value || slices.Contains(definition.ExpectedTaskDefinitions, value) {
			return true
		}
	}
	return false
}

func terminal(operation, key string, err error) error {
	return &adapter.Error{Class: adapter.ErrorTerminal, Operation: operation, CorrelationID: adapter.CorrelationID(key), Err: err}
}
func retryable(operation, key string, err error) error {
	return &adapter.Error{Class: adapter.ErrorRetryable, Operation: operation, CorrelationID: adapter.CorrelationID(key), Err: err}
}
func unknown(operation, key string, err error) error {
	return &adapter.Error{Class: adapter.ErrorUnknown, Operation: operation, CorrelationID: adapter.CorrelationID(key), Err: err}
}
