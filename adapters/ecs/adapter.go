package ecs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"themisy/pkg/adapter"
	appcredentials "themisy/pkg/credentials"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
)

const (
	Name    = "aws-ecs"
	Version = "v1alpha1"
)

type API interface {
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
	return &Adapter{config: config, clients: clients, now: time.Now}, nil
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
		return a.deployment(request.IdempotencyKey), nil
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
	return a.deployment(request.IdempotencyKey), nil
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
		return a.deployment(request.IdempotencyKey), nil
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
	return a.deployment(request.IdempotencyKey), nil
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
	output, err := client.DescribeServices(ctx, &awsecs.DescribeServicesInput{Cluster: aws.String(target.ClusterARN), Services: []string{target.ServiceARN}})
	if err != nil {
		return "", err
	}
	if output == nil || len(output.Failures) != 0 || len(output.Services) != 1 || output.Services[0].TaskDefinition == nil {
		return "", errors.New("ECS DescribeServices returned an ambiguous service set")
	}
	return aws.ToString(output.Services[0].TaskDefinition), nil
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
