package grant

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"themisy/pkg/protocol"
)

func TestVerifierRejectsExpiredModifiedAndWrongAudienceGrants(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	signer, publicKey, err := NewDevelopmentSigner("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	base, err := SignActionGrant(ctx, signer, grantFixture(now))
	if err != nil {
		t.Fatal(err)
	}
	verifier := &Verifier{Issuer: base.Issuer, RunnerGroup: base.RunnerGroup, TenantID: base.TenantID, Keys: StaticKeys{base.Issuer + "\x00dev-1": publicKey}, Now: func() time.Time { return now }}
	if err := verifier.Verify(ctx, base); err != nil {
		t.Fatalf("valid grant: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*protocol.ActionGrant)
		target error
	}{
		{"expired", func(g *protocol.ActionGrant) { g.ExpiresAt = now.Add(-time.Second); resign(t, ctx, signer, g) }, ErrExpired},
		{"target modified", func(g *protocol.ActionGrant) { g.Target.Service = "payments" }, ErrInvalidSignature},
		{"artifact modified", func(g *protocol.ActionGrant) { g.Action.ArtifactDigest = digest("b") }, ErrInvalidSignature},
		{"wrong audience", func(g *protocol.ActionGrant) { g.RunnerGroup = "other"; resign(t, ctx, signer, g) }, ErrWrongAudience},
		{"wrong tenant", func(g *protocol.ActionGrant) { g.TenantID = "other"; resign(t, ctx, signer, g) }, ErrWrongTenant},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate)
			if err := verifier.Verify(ctx, candidate); !errors.Is(err, test.target) {
				t.Fatalf("got %v, want %v", err, test.target)
			}
		})
	}
}

func resign(t *testing.T, ctx context.Context, signer Signer, actionGrant *protocol.ActionGrant) {
	t.Helper()
	signed, err := SignActionGrant(ctx, signer, *actionGrant)
	if err != nil {
		t.Fatal(err)
	}
	*actionGrant = signed
}

func digest(value string) string { return "sha256:" + strings.Repeat(value, 64) }

func grantFixture(now time.Time) protocol.ActionGrant {
	d := digest("a")
	return protocol.ActionGrant{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: "grant-1", Issuer: "https://control.example", TenantID: "tenant-1", RunnerGroup: "prod-jp", RunID: "run-1", StepID: "step-1", SubjectType: "user", UserSubject: "user-1", UserIdentityProof: "jwt", AgentID: "agent-1", DelegationRef: "delegation-1", Target: protocol.Target{Service: "identity", Environment: "production"}, Action: protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: d}, Risk: "medium", PlanHash: d, ContractHash: d, ProfileHash: d, PolicyHash: d, PolicyInputHash: d, EvidenceHash: d, ApprovalProofs: []string{"approval-1"}, IdempotencyKey: "key-1", Nonce: "nonce-1", IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)}
}

func TestV2LargeSignedPayloadAndStrictDeadline(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	signer, key, err := NewDevelopmentSigner("v2")
	if err != nil {
		t.Fatal(err)
	}
	g := grantFixture(now)
	legacy, err := protocol.GrantSigningMessage(g)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := protocol.CanonicalGrantPayload(g)
	if string(legacy) != string(canonical) {
		t.Fatal("legacy signature payload changed")
	}
	g.ProtocolVersion = protocol.VersionV1Alpha2
	g.AuthorizationEpoch = digest("b")
	g.AuthorizationValidUntil = g.ExpiresAt.Format(time.RFC3339Nano)
	g.UserIdentityProof = strings.Repeat("receipt", 1000)
	canonical, err = protocol.CanonicalGrantPayload(g)
	if err != nil || len(canonical) <= 4096 {
		t.Fatalf("large fixture: %v", err)
	}
	message, err := protocol.GrantSigningMessage(g)
	if err != nil || len(message) >= 4096 {
		t.Fatalf("unbounded signing message: %v", err)
	}
	g, err = SignActionGrant(ctx, signer, g)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Issuer: g.Issuer, RunnerGroup: g.RunnerGroup, TenantID: g.TenantID, Keys: StaticKeys{g.Issuer + "\x00v2": key}, ClockSkew: time.Minute, Now: func() time.Time { return now }}
	if err := v.Verify(ctx, g); err != nil {
		t.Fatal(err)
	}
	changed := g
	changed.UserIdentityProof += "modified"
	if !errors.Is(v.Verify(ctx, changed), ErrInvalidSignature) {
		t.Fatal("large receipt modification accepted")
	}
	changed = g
	changed.AuthorizationEpoch = digest("c")
	if !errors.Is(v.Verify(ctx, changed), ErrInvalidSignature) {
		t.Fatal("epoch modification accepted")
	}
	now = g.ExpiresAt.Add(-time.Nanosecond)
	if err := v.Verify(ctx, g); err != nil {
		t.Fatal(err)
	}
	now = g.ExpiresAt
	if !errors.Is(v.Verify(ctx, g), ErrExpired) {
		t.Fatal("clock skew extended authorization")
	}
}
