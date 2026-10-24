package ecs

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"strings"
)

// Resolve the configured immutable revision before any write. An ARN mapping
// alone is not evidence that its actual container images match the allowlist.
func verifyTaskDefinition(ctx context.Context, client API, arn string, expected map[string]string, key string) error {
	output, err := client.DescribeTaskDefinition(ctx, &awsecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(arn)})
	if err != nil {
		return retryable("describe-task-definition", key, errors.New("ECS task definition read unavailable"))
	}
	if ctx.Err() != nil {
		return retryable("describe-task-definition", key, errors.New("ECS task definition read cancelled"))
	}
	invalid := func() error {
		return terminal("task-definition-content", key, errors.New("ECS task definition does not match trusted container images"))
	}
	if output == nil || output.TaskDefinition == nil || !validContainers(expected) {
		return invalid()
	}
	definition := output.TaskDefinition
	if aws.ToString(definition.TaskDefinitionArn) != arn || definition.Status != types.TaskDefinitionStatusActive || len(definition.ContainerDefinitions) != len(expected) {
		return invalid()
	}
	seen := make(map[string]bool, len(expected))
	for _, container := range definition.ContainerDefinitions {
		name := aws.ToString(container.Name)
		digest, ok := expected[name]
		image := aws.ToString(container.Image)
		parts := strings.Split(image, "@")
		if !ok || seen[name] || len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(image) != image || parts[1] != digest {
			return invalid()
		}
		seen[name] = true
	}
	return nil
}
