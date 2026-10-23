package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestAWSBrokerUsesTrustedRoleAndRecordsGrantInSession(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	client := &fakeSTS{output: &sts.AssumeRoleOutput{Credentials: &types.Credentials{AccessKeyId: aws.String("access"), SecretAccessKey: aws.String("secret"), SessionToken: aws.String("session"), Expiration: aws.Time(now.Add(20 * time.Minute))}}}
	key := AWSRoleKey{TenantID: "tenant-1", Service: "payment-api", Environment: "production", Purpose: PurposeDeploy}
	broker := &AWSBroker{Now: func() time.Time { return now }, Client: client, Roles: map[AWSRoleKey]AWSRole{key: {RoleARN: "arn:aws:iam::111122223333:role/themisy-payment", ExternalID: "customer-external-id", Duration: 20 * time.Minute}}}
	credential, err := broker.Acquire(context.Background(), Request{Provider: ProviderAWS, TenantID: key.TenantID, RunID: "run-42", Service: key.Service, Environment: key.Environment, Purpose: key.Purpose, GrantID: "grant_01/test"})
	if err != nil {
		t.Fatal(err)
	}
	if string(credential.Value(AWSAccessKeyID)) != "access" || !credential.ValidAt(now) {
		t.Fatal("temporary credentials were not returned")
	}
	if aws.ToString(client.input.RoleArn) != broker.Roles[key].RoleARN || aws.ToString(client.input.RoleSessionName) != "themisy-grant_01-test" {
		t.Fatalf("assume role input=%#v", client.input)
	}
	if aws.ToString(client.input.ExternalId) != "customer-external-id" || aws.ToInt32(client.input.DurationSeconds) != 1200 {
		t.Fatalf("external ID or duration missing: %#v", client.input)
	}
	wantTags := map[string]string{AWSGrantTagKey: "grant_01/test", AWSRunTagKey: "run-42", AWSServiceTagKey: "payment-api", AWSEnvironmentTagKey: "production"}
	if len(client.input.Tags) != len(wantTags) {
		t.Fatalf("tags=%#v", client.input.Tags)
	}
	for _, tag := range client.input.Tags {
		key, value := aws.ToString(tag.Key), aws.ToString(tag.Value)
		if wantTags[key] != value {
			t.Fatalf("tag %q=%q, want %q", key, value, wantTags[key])
		}
		delete(wantTags, key)
	}
	if len(wantTags) != 0 {
		t.Fatalf("missing session tags: %#v", wantTags)
	}
}

func TestAWSBrokerCannotSelectRoleFromGrantFields(t *testing.T) {
	client := &fakeSTS{}
	broker := &AWSBroker{Client: client, Roles: map[AWSRoleKey]AWSRole{}}
	_, err := broker.Acquire(context.Background(), Request{Provider: ProviderAWS, TenantID: "tenant-1", RunID: "run-1", Service: "arn:aws:iam::999999999999:role/attacker", Environment: "production", Purpose: PurposeDeploy, GrantID: "grant-1"})
	if err == nil || client.input != nil {
		t.Fatalf("err=%v input=%#v", err, client.input)
	}
}

func TestAWSBrokerRejectsMissingRunIDBeforeSTS(t *testing.T) {
	client := &fakeSTS{}
	key := AWSRoleKey{TenantID: "tenant-1", Service: "payment-api", Environment: "production", Purpose: PurposeDeploy}
	broker := &AWSBroker{Client: client, Roles: map[AWSRoleKey]AWSRole{key: {RoleARN: "arn:aws:iam::111122223333:role/themisy-payment"}}}
	_, err := broker.Acquire(context.Background(), Request{Provider: ProviderAWS, TenantID: key.TenantID, Service: key.Service, Environment: key.Environment, Purpose: key.Purpose, GrantID: "grant-1"})
	if err == nil || client.input != nil {
		t.Fatalf("missing RunID reached STS: err=%v input=%#v", err, client.input)
	}
}

type fakeSTS struct {
	input  *sts.AssumeRoleInput
	output *sts.AssumeRoleOutput
	err    error
	hook   func()
}

func (f *fakeSTS) AssumeRole(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.input = input
	if f.hook != nil {
		f.hook()
	}
	return f.output, f.err
}

func TestAWSBrokerRejectsInvalidResponsesWithoutReturningCredentialBytes(t *testing.T) {
	for _, mode := range []string{"empty-access", "empty-secret", "empty-session", "expired", "too-long", "cancelled", "provider-error", "fractional-duration"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
			out := &sts.AssumeRoleOutput{Credentials: &types.Credentials{AccessKeyId: aws.String("secret-access"), SecretAccessKey: aws.String("secret-value"), SessionToken: aws.String("secret-token"), Expiration: aws.Time(now.Add(15 * time.Minute))}}
			client := &fakeSTS{output: out}
			key := AWSRoleKey{TenantID: "tenant", Service: "service", Environment: "staging", Purpose: PurposeDeploy}
			role := AWSRole{RoleARN: "arn:aws:iam::111122223333:role/test", Duration: 15 * time.Minute}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "empty-access":
				out.Credentials.AccessKeyId = aws.String(" ")
			case "empty-secret":
				out.Credentials.SecretAccessKey = aws.String("")
			case "empty-session":
				out.Credentials.SessionToken = aws.String("")
			case "expired":
				out.Credentials.Expiration = aws.Time(now)
			case "too-long":
				out.Credentials.Expiration = aws.Time(now.Add(time.Hour))
			case "cancelled":
				client.hook = cancel
			case "provider-error":
				client.err = errors.New("secret-value provider body")
			case "fractional-duration":
				role.Duration += time.Nanosecond
			}
			b := AWSBroker{Client: client, Roles: map[AWSRoleKey]AWSRole{key: role}, Now: func() time.Time { return now }}
			credential, err := b.Acquire(ctx, Request{Provider: ProviderAWS, TenantID: key.TenantID, Service: key.Service, Environment: key.Environment, Purpose: key.Purpose, RunID: "run", GrantID: "grant"})
			if err == nil || len(credential.Value(AWSAccessKeyID)) != 0 || credential.ValidAt(now) || strings.Contains(err.Error(), "secret-value") {
				t.Fatal("invalid response exposed credentials or diagnostics")
			}
			if mode == "fractional-duration" && client.input != nil {
				t.Fatal("fractional duration reached STS")
			}
		})
	}
}
