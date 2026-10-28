package runner

import (
	"context"
	"testing"
	"themisy/internal/identity"
	"themisy/pkg/protocol"
)

type credentialHook struct {
	base *countingCredentials
	hook func()
}

func (c credentialHook) Acquire(ctx context.Context, r CredentialRequest) (Credential, error) {
	v, err := c.base.Acquire(ctx, r)
	c.hook()
	return v, err
}
func TestIdentityRevokedDuringCredentialAcquisitionPreventsWrite(t *testing.T) {
	f := newRunnerFixture(t)
	f.runner.Credentials = credentialHook{base: f.credentials, hook: func() { f.runner.Subjects = staticSubjectVerifier{err: identity.ErrInvalidIdentityProof} }}
	_, err := f.runner.Execute(context.Background(), f.grant)
	if err == nil || f.credentials.Calls() != 1 || f.adapter.Calls() != 0 {
		t.Fatalf("err=%v credentials=%d writes=%d", err, f.credentials.Calls(), f.adapter.Calls())
	}
}
func TestDelegationRevokedDuringCredentialAcquisitionPreventsWrite(t *testing.T) {
	f := newRunnerFixture(t)
	f.runner.Credentials = credentialHook{base: f.credentials, hook: func() { f.runner.Delegations = &identity.DelegationVerifier{Resolver: identity.StaticDelegations{}} }}
	result, err := f.runner.Execute(context.Background(), f.grant)
	if err == nil || result.ReasonCode != string(ReasonDelegation) || f.adapter.Calls() != 0 {
		t.Fatalf("result=%+v err=%v writes=%d", result, err, f.adapter.Calls())
	}
}
func TestInitialIdentityFailureObtainsNoCredentialAndMakesNoWrite(t *testing.T) {
	f := newRunnerFixture(t)
	f.runner.Subjects = staticSubjectVerifier{err: identity.ErrInvalidIdentityProof}
	result, err := f.runner.Execute(context.Background(), f.grant)
	if err == nil || result.Status != protocol.ResultRejected || f.credentials.Calls() != 0 || f.adapter.Calls() != 0 {
		t.Fatalf("result=%+v err=%v credentials=%d writes=%d", result, err, f.credentials.Calls(), f.adapter.Calls())
	}
}
