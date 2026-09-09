package credentials

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

const (
	ProviderAWS        = "aws"
	AWSAccessKeyID     = "aws_access_key_id"
	AWSSecretAccessKey = "aws_secret_access_key"
	AWSSessionToken    = "aws_session_token"
	AWSGrantTagKey     = "ThemisyGrantID"
)

type AWSRoleKey struct {
	TenantID    string
	Service     string
	Environment string
	Purpose     Purpose
}

type AWSRole struct {
	RoleARN    string
	ExternalID string
	Duration   time.Duration
}

type STSAPI interface {
	AssumeRole(context.Context, *sts.AssumeRoleInput, ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// AWSBroker exchanges the Runner's workload identity for short-lived role
// credentials. Role ARNs are selected only from Runner-local trusted config.
type AWSBroker struct {
	Client STSAPI
	Roles  map[AWSRoleKey]AWSRole
}

func (b *AWSBroker) Acquire(ctx context.Context, request Request) (Credential, error) {
	if err := request.Validate(); err != nil {
		return Credential{}, err
	}
	if request.Provider != ProviderAWS || request.GrantID == "" || b.Client == nil {
		return Credential{}, errors.New("AWS workload credential request is incomplete")
	}
	role, ok := b.Roles[AWSRoleKey{TenantID: request.TenantID, Service: request.Service, Environment: request.Environment, Purpose: request.Purpose}]
	if !ok || role.RoleARN == "" {
		return Credential{}, errors.New("AWS role is not allow-listed for this target")
	}
	duration := role.Duration
	if duration <= 0 {
		duration = 15 * time.Minute
	}
	if duration < 15*time.Minute || duration > 12*time.Hour {
		return Credential{}, errors.New("AWS role duration must be between 15m and 12h")
	}
	seconds := int32(duration / time.Second)
	input := &sts.AssumeRoleInput{
		RoleArn:         aws.String(role.RoleARN),
		RoleSessionName: aws.String(sessionName(request.GrantID)),
		DurationSeconds: aws.Int32(seconds),
		Tags:            []types.Tag{{Key: aws.String(AWSGrantTagKey), Value: aws.String(request.GrantID)}},
	}
	if role.ExternalID != "" {
		input.ExternalId = aws.String(role.ExternalID)
	}
	output, err := b.Client.AssumeRole(ctx, input)
	if err != nil {
		return Credential{}, fmt.Errorf("assume allow-listed AWS role: %w", err)
	}
	if output == nil || output.Credentials == nil || output.Credentials.AccessKeyId == nil || output.Credentials.SecretAccessKey == nil || output.Credentials.SessionToken == nil || output.Credentials.Expiration == nil {
		return Credential{}, errors.New("STS returned incomplete temporary credentials")
	}
	return New(map[string][]byte{
		AWSAccessKeyID:     []byte(aws.ToString(output.Credentials.AccessKeyId)),
		AWSSecretAccessKey: []byte(aws.ToString(output.Credentials.SecretAccessKey)),
		AWSSessionToken:    []byte(aws.ToString(output.Credentials.SessionToken)),
	}, aws.ToTime(output.Credentials.Expiration)), nil
}

var invalidSessionName = regexp.MustCompile(`[^A-Za-z0-9_+=,.@-]`)

func sessionName(grantID string) string {
	value := "themisy-" + invalidSessionName.ReplaceAllString(strings.TrimSpace(grantID), "-")
	if len(value) > 64 {
		value = value[:64]
	}
	if len(value) < 2 {
		return "themisy-grant"
	}
	return value
}
