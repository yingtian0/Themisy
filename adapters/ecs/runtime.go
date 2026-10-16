package ecs

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"themisy/pkg/adapter"
)

// A single observation is bounded. Incomplete rollouts stay UNKNOWN and are
// recovered only through read-only Reconcile; this never polls or resends writes.
const maxRuntimeTasks = 1000
const maxTaskPages = 10

func (a *Adapter) confirmRuntime(ctx context.Context, client API, target Target, definition string, containers map[string]string, key string) (adapter.Deployment, error) {
	if err := observeRuntime(ctx, client, target, definition, containers); err != nil {
		return adapter.Deployment{}, unknown("runtime-verification", key, err)
	}
	return a.deployment(key), nil
}

func observeRuntime(ctx context.Context, client API, target Target, definition string, containers map[string]string) error {
	if !validContainers(containers) {
		return errors.New("trusted container expectations are missing")
	}
	service, err := describeService(ctx, client, target)
	if err != nil {
		return err
	}
	if aws.ToString(service.Status) != "ACTIVE" || aws.ToString(service.TaskDefinition) != definition || service.DesiredCount <= 0 || service.DesiredCount > maxRuntimeTasks || service.RunningCount != service.DesiredCount || service.PendingCount != 0 || len(service.Deployments) != 1 {
		return errors.New("ECS service has not converged")
	}
	primary := service.Deployments[0]
	if aws.ToString(primary.Status) != "PRIMARY" || aws.ToString(primary.TaskDefinition) != definition || primary.RolloutState != types.DeploymentRolloutStateCompleted || primary.DesiredCount != service.DesiredCount || primary.RunningCount != service.DesiredCount || primary.PendingCount != 0 {
		return errors.New("ECS primary rollout is not complete")
	}
	serviceName := target.ServiceARN[strings.LastIndex(target.ServiceARN, "/")+1:]
	if serviceName == "" || serviceName == target.ServiceARN {
		return errors.New("invalid trusted service ARN")
	}
	var arns []string
	seen := map[string]bool{}
	tokens := map[string]bool{}
	var token *string
	for page := 0; ; page++ {
		if page >= maxTaskPages {
			return errors.New("ECS task pagination bound exceeded")
		}
		output, err := client.ListTasks(ctx, &awsecs.ListTasksInput{Cluster: aws.String(target.ClusterARN), ServiceName: aws.String(serviceName), DesiredStatus: types.DesiredStatusRunning, MaxResults: aws.Int32(100), NextToken: token})
		if err != nil {
			return err
		}
		if output == nil {
			return errors.New("missing ECS task list")
		}
		for _, arn := range output.TaskArns {
			if arn == "" || seen[arn] {
				return errors.New("duplicate or empty ECS task ARN")
			}
			seen[arn] = true
			arns = append(arns, arn)
			if len(arns) > maxRuntimeTasks || len(arns) > int(service.DesiredCount) {
				return errors.New("unexpected ECS task count")
			}
		}
		if aws.ToString(output.NextToken) == "" {
			break
		}
		if tokens[*output.NextToken] {
			return errors.New("repeated ECS task pagination token")
		}
		tokens[*output.NextToken] = true
		token = output.NextToken
	}
	if len(arns) != int(service.DesiredCount) {
		return errors.New("incomplete ECS task list")
	}
	for start := 0; start < len(arns); start += 100 {
		end := min(start+100, len(arns))
		requested := map[string]bool{}
		for _, arn := range arns[start:end] {
			requested[arn] = true
		}
		output, err := client.DescribeTasks(ctx, &awsecs.DescribeTasksInput{Cluster: aws.String(target.ClusterARN), Tasks: arns[start:end]})
		if err != nil {
			return err
		}
		if output == nil || len(output.Failures) != 0 || len(output.Tasks) != len(requested) {
			return errors.New("incomplete ECS task response")
		}
		for _, task := range output.Tasks {
			arn := aws.ToString(task.TaskArn)
			if !requested[arn] {
				return errors.New("duplicate or foreign ECS task response")
			}
			delete(requested, arn)
			if aws.ToString(task.ClusterArn) != target.ClusterARN || aws.ToString(task.Group) != "service:"+serviceName || aws.ToString(task.TaskDefinitionArn) != definition || aws.ToString(task.DesiredStatus) != "RUNNING" || aws.ToString(task.LastStatus) != "RUNNING" || task.HealthStatus != types.HealthStatusHealthy || len(task.Containers) != len(containers) {
				return errors.New("ECS task runtime is not healthy or trusted")
			}
			names := map[string]bool{}
			for _, container := range task.Containers {
				name := aws.ToString(container.Name)
				digest, ok := containers[name]
				if !ok || names[name] || aws.ToString(container.TaskArn) != arn || aws.ToString(container.LastStatus) != "RUNNING" || container.HealthStatus != types.HealthStatusHealthy || aws.ToString(container.ImageDigest) != digest {
					return errors.New("ECS container runtime is not healthy or trusted")
				}
				names[name] = true
			}
		}
	}
	return nil
}
