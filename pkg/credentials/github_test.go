package credentials

import (
	"context"
	"testing"
	"time"
)

func TestGitHubBrokerEnforcesLocalTargetAllowlist(t *testing.T) {
	now := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	target := GitHubTarget{TenantID: "tenant-1", Service: "identity", Environment: "production"}
	broker := &GitHubBroker{Source: staticGitHubSource{credential: New(map[string][]byte{"token": []byte("secret")}, now.Add(time.Hour))}, Allowed: map[GitHubTarget]struct{}{target: {}}, Now: func() time.Time { return now }}
	credential, err := broker.Acquire(context.Background(), Request{Provider: GitHubProvider, TenantID: target.TenantID, Service: target.Service, Environment: target.Environment, Purpose: PurposeDeploy})
	if err != nil || string(credential.Value("token")) != "secret" {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	if _, err := broker.Acquire(context.Background(), Request{Provider: GitHubProvider, TenantID: target.TenantID, Service: "payments", Environment: target.Environment, Purpose: PurposeDeploy}); err == nil {
		t.Fatal("credential escaped target allowlist")
	}
	copy := credential.Value("token")
	copy[0] = 'X'
	if string(credential.Value("token")) != "secret" {
		t.Fatal("credential value was not copied")
	}
}

func TestGitHubBrokerAllowsOnlyAllowlistedReadOnlyVerification(t *testing.T) {
	now := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	target := GitHubTarget{TenantID: "tenant-1", Service: "identity", Environment: "staging"}
	broker := &GitHubBroker{Source: staticGitHubSource{credential: New(map[string][]byte{"token": []byte("read-token")}, now.Add(time.Hour))}, Allowed: map[GitHubTarget]struct{}{target: {}}, Now: func() time.Time { return now }}
	request := Request{Provider: GitHubProvider, TenantID: target.TenantID, Service: target.Service, Environment: target.Environment, Purpose: PurposeVerify}
	credential, err := broker.Acquire(context.Background(), request)
	if err != nil || string(credential.Value("token")) != "read-token" {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	request.Environment = "production"
	if _, err := broker.Acquire(context.Background(), request); err == nil {
		t.Fatal("read credential escaped its local target allowlist")
	}
}

type staticGitHubSource struct{ credential Credential }

func (s staticGitHubSource) Token(context.Context) (Credential, error) {
	return s.credential.Clone(), nil
}
