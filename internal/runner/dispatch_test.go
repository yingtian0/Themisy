package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

func TestSDKDispatcherDoesNotReturnCredentialAndUnknownIsNotRetried(t *testing.T) {
	fixture := newRunnerFixture(t)
	provider := &sdkProbe{err: &adapter.Error{Class: adapter.ErrorUnknown, Operation: "deploy", Err: errors.New("timeout")}}
	fixture.runner.Adapter = &SDKDispatcher{Adapter: provider, Now: func() time.Time { return fixture.now }}
	first, err := fixture.runner.Execute(context.Background(), fixture.grant)
	if err == nil || first.Status != "UNKNOWN" {
		t.Fatalf("result=%#v err=%v", first, err)
	}
	second, err := fixture.runner.Execute(context.Background(), fixture.grant)
	if err == nil || second.Status != "RECONCILIATION_REQUIRED" {
		t.Fatalf("replay=%#v err=%v", second, err)
	}
	if provider.deployCalls != 1 || fixture.credentials.Calls() != 1 {
		t.Fatalf("deploy=%d credentials=%d", provider.deployCalls, fixture.credentials.Calls())
	}
	encoded, _ := json.Marshal(first)
	if strings.Contains(string(encoded), "redacted") || strings.Contains(string(encoded), "token") {
		t.Fatalf("credential leaked in protocol result: %s", encoded)
	}
}

type sdkProbe struct {
	deployCalls int
	err         error
}

func (*sdkProbe) Name() string                           { return "probe" }
func (*sdkProbe) Version() string                        { return "probe/v1" }
func (*sdkProbe) Authorize(adapter.Target, string) error { return nil }
func (p *sdkProbe) Deploy(_ context.Context, _ adapter.DeployRequest, credential credentials.Credential) (adapter.Deployment, error) {
	p.deployCalls++
	if len(credential.Value("token")) == 0 {
		return adapter.Deployment{}, errors.New("credential missing")
	}
	return adapter.Deployment{}, p.err
}
func (*sdkProbe) Rollback(context.Context, adapter.RollbackRequest, credentials.Credential) (adapter.Deployment, error) {
	return adapter.Deployment{}, errors.New("not used")
}
func (*sdkProbe) Reconcile(context.Context, adapter.ReconcileRequest, credentials.Credential) (adapter.ReconcileResult, error) {
	return adapter.ReconcileResult{}, errors.New("not used")
}
