package credentials

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

func TestAWSBrokerUsesTrustedRoleAndRecordsGrantInSession(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	client := &fakeSTS{output: &sts.AssumeRoleOutput{Credentials: &types.Credentials{AccessKeyId: aws.String("access"), SecretAccessKey: aws.String("secret"), SessionToken: aws.String("session"), Expiration: aws.Time(now.Add(time.Hour))}}}
	key := AWSRoleKey{TenantID: "tenant-1", Service: "payment-api", Environment: "production", Purpose: PurposeDeploy}
	broker := &AWSBroker{Client: client, Roles: map[AWSRoleKey]AWSRole{key: {RoleARN: "arn:aws:iam::111122223333:role/themisy-payment", Duration: 20 * time.Minute}}}
	credential, err := broker.Acquire(context.Background(), Request{Provider: ProviderAWS, TenantID: key.TenantID, Service: key.Service, Environment: key.Environment, Purpose: key.Purpose, GrantID: "grant_01/test"})
	if err != nil {
		t.Fatal(err)
	}
	if string(credential.Value(AWSAccessKeyID)) != "access" || !credential.ValidAt(now) {
		t.Fatal("temporary credentials were not returned")
	}
	if aws.ToString(client.input.RoleArn) != broker.Roles[key].RoleARN || aws.ToString(client.input.RoleSessionName) != "themisy-grant_01-test" {
		t.Fatalf("assume role input=%#v", client.input)
	}
	if len(client.input.Tags) != 1 || aws.ToString(client.input.Tags[0].Key) != AWSGrantTagKey || aws.ToString(client.input.Tags[0].Value) != "grant_01/test" {
		t.Fatalf("tags=%#v", client.input.Tags)
	}
}

func TestAWSBrokerCannotSelectRoleFromGrantFields(t *testing.T) {
	client := &fakeSTS{}
	broker := &AWSBroker{Client: client, Roles: map[AWSRoleKey]AWSRole{}}
	_, err := broker.Acquire(context.Background(), Request{Provider: ProviderAWS, TenantID: "tenant-1", Service: "arn:aws:iam::999999999999:role/attacker", Environment: "production", Purpose: PurposeDeploy, GrantID: "grant-1"})
	if err == nil || client.input != nil {
		t.Fatalf("err=%v input=%#v", err, client.input)
	}
}

type fakeSTS struct {
	input  *sts.AssumeRoleInput
	output *sts.AssumeRoleOutput
}

func (f *fakeSTS) AssumeRole(_ context.Context, input *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.input = input
	return f.output, nil
}
