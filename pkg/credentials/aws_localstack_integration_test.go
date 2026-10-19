//go:build integration

package credentials

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type localStackSTSCalls struct {
	client *sts.Client
	calls  atomic.Int32
}

func (c *localStackSTSCalls) AssumeRole(ctx context.Context, input *sts.AssumeRoleInput, options ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	c.calls.Add(1)
	return c.client.AssumeRole(ctx, input, options...)
}

func TestLocalStackSTSBrokerRoundTrip(t *testing.T) {
	raw := os.Getenv("THEMISY_LOCALSTACK_STS_ENDPOINT")
	if raw == "" {
		t.Skip("requires an authenticated dedicated LocalStack endpoint and fixed fixture role; not an AWS staging pass")
	}
	endpoint, err := localStackEndpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/_localstack/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal("dedicated LocalStack health unavailable")
	}
	var health struct {
		Services map[string]string `json:"services"`
		Version  string            `json:"version"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || health.Version == "" || (health.Services["sts"] != "available" && health.Services["sts"] != "running") {
		t.Fatal("LocalStack STS is not ready; missing/unavailable is not PASS")
	}
	// Explicit synthetic credentials: never load AWS_PROFILE, SSO, environment
	// credentials, an instance role or any other customer workload identity.
	base := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test", Source: "synthetic-localstack-fixture"}, nil
	})
	options := sts.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), Credentials: base, HTTPClient: httpClient, Retryer: aws.NopRetryer{}}
	counter := &localStackSTSCalls{client: sts.New(options)}
	key := AWSRoleKey{TenantID: "localstack-fixture", Service: "identity", Environment: "staging", Purpose: PurposeDeploy}
	const role = "arn:aws:iam::000000000000:role/themisy-localstack-sts-fixture"
	broker := &AWSBroker{Client: counter, Roles: map[AWSRoleKey]AWSRole{key: {RoleARN: role, ExternalID: "themisy-localstack-fixture", Duration: 15 * time.Minute}}}
	input := Request{Provider: ProviderAWS, TenantID: key.TenantID, RunID: "localstack-run", Service: key.Service, Environment: key.Environment, Purpose: key.Purpose, GrantID: "localstack-grant"}
	credential, err := broker.Acquire(ctx, input)
	// Do not print provider messages or response objects containing credentials.
	if err != nil {
		t.Fatal("LocalStack AssumeRole failed; no credential or provider-response body logged")
	}
	now := time.Now().UTC()
	if !credential.ValidAt(now) || credential.ExpiresAt.After(now.Add(16*time.Minute)) || len(credential.Value(AWSAccessKeyID)) == 0 || len(credential.Value(AWSSecretAccessKey)) == 0 || len(credential.Value(AWSSessionToken)) == 0 {
		t.Fatal("LocalStack did not return complete short-lived credentials")
	}
	options.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: string(credential.Value(AWSAccessKeyID)), SecretAccessKey: string(credential.Value(AWSSecretAccessKey)), SessionToken: string(credential.Value(AWSSessionToken)), Expires: credential.ExpiresAt, CanExpire: true, Source: "synthetic-localstack-assumed-role"}, nil
	})
	identity, err := sts.New(options).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal("LocalStack assumed-role identity lookup failed; no credential or provider-response body logged")
	}
	expectedARN := "arn:aws:sts::000000000000:assumed-role/themisy-localstack-sts-fixture/" + sessionName(input.GrantID)
	if aws.ToString(identity.Arn) != expectedARN || aws.ToString(identity.Account) != "000000000000" {
		t.Fatal("LocalStack identity does not match the fixed role/session")
	}
	for _, negative := range []string{"tenant", "service-arn", "rollback"} {
		denied := input
		switch negative {
		case "tenant":
			denied.TenantID = "unmapped-tenant"
		case "service-arn":
			denied.Service = "arn:aws:iam::999999999999:role/unmapped"
		case "rollback":
			denied.Purpose = PurposeRollback
		}
		if _, err := broker.Acquire(ctx, denied); err == nil {
			t.Fatalf("unmapped request %s accepted", negative)
		}
	}
	if counter.calls.Load() != 1 {
		t.Fatal("unmapped request reached STS or AssumeRole was repeated")
	}
}
