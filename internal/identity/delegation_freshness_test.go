package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"themisy/pkg/protocol"
	"time"
)

type delegationLookup struct {
	record Delegation
	err    error
	calls  int
	hook   func()
}

func (d *delegationLookup) ResolveDelegation(context.Context, string) (Delegation, error) {
	d.calls++
	if d.hook != nil {
		d.hook()
	}
	return d.record, d.err
}
func delegationFreshnessFixture() (*DelegationVerifier, *delegationLookup, DelegationRequest, time.Time) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	lookup := &delegationLookup{record: Delegation{ID: "delegation", Issuer: "issuer", UserSubject: "user", AgentID: "agent", Actions: []protocol.Capability{protocol.CapabilityDeploy}, ServiceSelectors: []string{"service"}, Environments: []string{"staging"}, MaximumRisk: "medium", ExpiresAt: now.Add(time.Hour)}}
	verifier := &DelegationVerifier{Resolver: lookup, Now: func() time.Time { return now }}
	request := DelegationRequest{Reference: "delegation", Subject: Subject{Issuer: "issuer", ID: "user", IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)}, AgentID: "agent", Capability: protocol.CapabilityDeploy, Service: "service", Environment: "staging", Risk: "low"}
	return verifier, lookup, request, now
}
func TestDelegationRechecksSubjectAfterTrustedLookup(t *testing.T) {
	for _, mode := range []string{"expired", "missing-expiry", "future-issued", "cancel-before", "cancel-during", "expires-during", "provider-error"} {
		t.Run(mode, func(t *testing.T) {
			verifier, lookup, request, now := delegationFreshnessFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "expired":
				request.Subject.ExpiresAt = now
			case "missing-expiry":
				request.Subject.ExpiresAt = time.Time{}
			case "future-issued":
				request.Subject.IssuedAt = now.Add(time.Second)
			case "cancel-before":
				cancel()
			case "cancel-during":
				lookup.hook = cancel
			case "expires-during":
				lookup.hook = func() { verifier.Now = func() time.Time { return request.Subject.ExpiresAt } }
			case "provider-error":
				lookup.err = errors.New("raw-token-secret-canary")
			}
			record, err := verifier.Verify(ctx, request)
			if !errors.Is(err, ErrDelegationDenied) || record.ID != "" || strings.Contains(err.Error(), "secret-canary") {
				t.Fatal("invalid subject/lookup returned delegation or raw error")
			}
			if mode == "cancel-before" && lookup.calls != 0 {
				t.Fatal("cancelled request reached directory lookup")
			}
		})
	}
}
func TestDelegationReturnedScopesDoNotAliasTrustedRecords(t *testing.T) {
	verifier, lookup, request, _ := delegationFreshnessFixture()
	record, err := verifier.Verify(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	record.Actions[0] = protocol.CapabilityRollback
	record.ServiceSelectors[0] = "*"
	record.Environments[0] = "production"
	if lookup.record.Actions[0] != protocol.CapabilityDeploy || lookup.record.ServiceSelectors[0] != "service" || lookup.record.Environments[0] != "staging" {
		t.Fatal("caller changed trusted delegation scopes")
	}
	static := StaticDelegations{lookup.record.ID: lookup.record}
	copy, err := static.ResolveDelegation(context.Background(), lookup.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	copy.ServiceSelectors[0] = "*"
	if static[lookup.record.ID].ServiceSelectors[0] != "service" {
		t.Fatal("static resolver exposes mutable scope")
	}
	if _, err := verifier.Verify(context.Background(), request); err != nil {
		t.Fatal("caller corrupted next authorization", err)
	}
}
