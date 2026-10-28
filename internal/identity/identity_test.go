package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"themisy/pkg/protocol"
)

func TestOIDCVerifierRequiresSignatureIssuerAudienceAndExpiry(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, audienceValue := "https://identity.example", "themisy"
	verifier := &OIDCVerifier{Issuer: issuer, Audience: audienceValue, Keys: StaticOIDCKeys{issuer + "\x00idp-1": public}, Now: func() time.Time { return now }}
	valid := signJWT(t, private, map[string]any{"iss": issuer, "sub": "user-1", "aud": audienceValue, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix()})
	subject, err := verifier.Verify(context.Background(), valid)
	if err != nil || subject.ID != "user-1" {
		t.Fatalf("subject=%#v err=%v", subject, err)
	}
	for name, claims := range map[string]map[string]any{
		"issuer":   {"iss": "https://evil.example", "sub": "user-1", "aud": audienceValue, "exp": now.Add(time.Minute).Unix()},
		"audience": {"iss": issuer, "sub": "user-1", "aud": "other", "exp": now.Add(time.Minute).Unix()},
		"expiry":   {"iss": issuer, "sub": "user-1", "aud": audienceValue, "exp": now.Add(-time.Second).Unix()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), signJWT(t, private, claims)); !errors.Is(err, ErrInvalidIdentityProof) {
				t.Fatalf("got %v", err)
			}
		})
	}
	parts := strings.Split(valid, ".")
	modifiedSignature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	modifiedSignature[0] ^= 0x01
	modified := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(modifiedSignature)
	if _, err := verifier.Verify(context.Background(), modified); !errors.Is(err, ErrInvalidIdentityProof) {
		t.Fatalf("modified: %v", err)
	}
}

func TestDelegationUsesTrustedRecordAndEnforcesScope(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	record := Delegation{ID: "delegation-1", Issuer: "issuer", UserSubject: "user-1", AgentID: "agent-1", Actions: []protocol.Capability{protocol.CapabilityDeploy}, ServiceSelectors: []string{"identity-*"}, Environments: []string{"staging"}, MaximumRisk: "medium", ExpiresAt: now.Add(time.Hour)}
	verifier := &DelegationVerifier{Resolver: StaticDelegations{record.ID: record}, Now: func() time.Time { return now }}
	request := DelegationRequest{Reference: record.ID, Subject: Subject{Issuer: "issuer", ID: "user-1", ExpiresAt: now.Add(time.Hour)}, AgentID: "agent-1", Capability: protocol.CapabilityDeploy, Service: "identity-api", Environment: "staging", Risk: "medium"}
	if _, err := verifier.Verify(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Environment = "production"
	if _, err := verifier.Verify(context.Background(), request); !errors.Is(err, ErrDelegationDenied) {
		t.Fatalf("got %v", err)
	}
}

func signJWT(t *testing.T, private ed25519.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "EdDSA", "kid": "idp-1", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	input := encodedHeader + "." + encodedPayload
	signature := ed25519.Sign(private, []byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestCriticalRiskRequiresExplicitCriticalDelegation(t *testing.T) {
	now := time.Now().UTC()
	d := Delegation{ID: "d", Issuer: "idp", UserSubject: "user", AgentID: "agent", Actions: []protocol.Capability{protocol.CapabilityDeploy}, ServiceSelectors: []string{"service"}, Environments: []string{"production"}, MaximumRisk: "high", ExpiresAt: now.Add(time.Hour)}
	records := StaticDelegations{"d": d}
	v := &DelegationVerifier{Resolver: records}
	request := DelegationRequest{Reference: "d", Subject: Subject{Issuer: "idp", ID: "user", ExpiresAt: now.Add(time.Hour)}, AgentID: "agent", Capability: protocol.CapabilityDeploy, Service: "service", Environment: "production", Risk: "critical"}
	if _, err := v.Verify(context.Background(), request); err == nil {
		t.Fatal("high delegation authorized critical")
	}
	d.MaximumRisk = "critical"
	records["d"] = d
	if _, err := v.Verify(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}
