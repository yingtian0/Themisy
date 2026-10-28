package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"themisy/internal/grant"
	"themisy/pkg/protocol"
	"time"
)

// Attestation carries no bearer token or provider credential. It is a narrowly
// scoped signed receipt of OIDC verification, not a reusable login credential.
type Attestation struct {
	Roles         []string           `json:"roles"`
	Version       string             `json:"version"`
	Issuer        string             `json:"issuer"`
	TenantID      string             `json:"tenant_id"`
	Audience      string             `json:"audience"`
	SubjectIssuer string             `json:"subject_issuer"`
	SubjectID     string             `json:"subject_id"`
	SubjectType   string             `json:"subject_type"`
	OIDCKeyID     string             `json:"oidc_key_id"`
	OIDCKeyHash   string             `json:"oidc_key_hash"`
	RequestID     string             `json:"request_id"`
	AgentID       string             `json:"agent_id"`
	DelegationRef string             `json:"delegation_ref"`
	IssuedAt      time.Time          `json:"issued_at"`
	ExpiresAt     time.Time          `json:"expires_at"`
	Signature     protocol.Signature `json:"signature"`
}

const attestationVersion = "themisy.identity-attestation/v1"
const attestationPrefix = "themisy-identity-v1:"

func attestationMessage(a Attestation) ([]byte, error) {
	a.Signature.Value = ""
	a.IssuedAt = a.IssuedAt.UTC()
	a.ExpiresAt = a.ExpiresAt.UTC()
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, ErrInvalidIdentityProof
	}
	sum := sha256.Sum256(raw)
	return []byte(attestationVersion + "\x00" + hex.EncodeToString(sum[:])), nil
}
func keyHash(key any) (string, error) {
	raw, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", ErrInvalidIdentityProof
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Attest verifies the raw token in memory. Only the resulting signed receipt
// may enter a durable Run, Plan, Action Grant, or workflow history.
func Attest(ctx context.Context, v *OIDCVerifier, signer grant.Signer, keyID string, a Attestation, token string) (string, error) {
	if v == nil || signer == nil || keyID == "" {
		return "", ErrInvalidIdentityProof
	}
	subject, err := v.Verify(ctx, token)
	if err != nil {
		return "", ErrInvalidIdentityProof
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ErrInvalidIdentityProof
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", ErrInvalidIdentityProof
	}
	var header jwtHeader
	if json.Unmarshal(raw, &header) != nil {
		return "", ErrInvalidIdentityProof
	}
	key, err := v.Keys.ResolveOIDCKey(ctx, v.Issuer, header.KeyID)
	if err != nil {
		return "", ErrInvalidIdentityProof
	}
	hash, err := keyHash(key)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	a.Version = attestationVersion
	a.SubjectIssuer = subject.Issuer
	a.SubjectID = subject.ID
	a.OIDCKeyID = header.KeyID
	a.OIDCKeyHash = hash
	a.IssuedAt = now
	a.ExpiresAt = now.Add(5 * time.Minute)
	if subject.ExpiresAt.Before(a.ExpiresAt) {
		a.ExpiresAt = subject.ExpiresAt
	}
	a.Signature = protocol.Signature{Algorithm: "Ed25519", KeyID: keyID}
	if !validAttestation(a, now) {
		return "", ErrInvalidIdentityProof
	}
	message, err := attestationMessage(a)
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(ctx, message)
	if err != nil || sig.Algorithm != "Ed25519" || sig.KeyID != keyID || sig.Value == "" || ctx.Err() != nil {
		return "", ErrInvalidIdentityProof
	}
	a.Signature = sig
	raw, err = json.Marshal(a)
	if err != nil {
		return "", err
	}
	return attestationPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}
func validAttestation(a Attestation, now time.Time) bool {
	return a.Version == attestationVersion && a.Issuer != "" && a.TenantID != "" && a.Audience != "" && a.SubjectIssuer != "" && a.SubjectID != "" && (a.SubjectType == "user" || a.SubjectType == "ci") && a.OIDCKeyID != "" && a.OIDCKeyHash != "" && a.RequestID != "" && a.AgentID != "" && a.DelegationRef != "" && !a.IssuedAt.IsZero() && !a.IssuedAt.After(now) && a.ExpiresAt.After(now) && a.ExpiresAt.Sub(a.IssuedAt) <= 5*time.Minute && a.Signature.Algorithm == "Ed25519" && a.Signature.KeyID != ""
}

type AttestationVerifier struct {
	Issuer, TenantID, Audience string
	Keys                       grant.KeyResolver
	OIDC                       *OIDCVerifier
	Directory                  *FileDirectory
	Now                        func() time.Time
}

func (v *AttestationVerifier) VerifyAttestation(ctx context.Context, proof string) (Attestation, error) {
	if v == nil || v.Keys == nil || v.OIDC == nil || v.OIDC.Keys == nil || v.Directory == nil || !strings.HasPrefix(proof, attestationPrefix) || len(proof) > 8192 || ctx.Err() != nil {
		return Attestation{}, ErrInvalidIdentityProof
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(proof, attestationPrefix))
	if err != nil {
		return Attestation{}, ErrInvalidIdentityProof
	}
	var a Attestation
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if dec.Decode(&a) != nil || dec.Decode(new(any)) != io.EOF {
		return Attestation{}, ErrInvalidIdentityProof
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	if !validAttestation(a, now) || a.Issuer != v.Issuer || a.TenantID != v.TenantID || a.Audience != v.Audience || a.SubjectIssuer != v.OIDC.Issuer || v.Directory.TenantID != v.TenantID {
		return Attestation{}, ErrInvalidIdentityProof
	}
	message, err := attestationMessage(a)
	if err != nil {
		return Attestation{}, err
	}
	key, err := v.Keys.ResolveGrantKey(ctx, a.Issuer, a.Signature.KeyID)
	if err != nil {
		return Attestation{}, ErrInvalidIdentityProof
	}
	sig, err := base64.RawURLEncoding.DecodeString(a.Signature.Value)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, message, sig) {
		return Attestation{}, ErrInvalidIdentityProof
	}
	oidcKey, err := v.OIDC.Keys.ResolveOIDCKey(ctx, a.SubjectIssuer, a.OIDCKeyID)
	if err != nil {
		return Attestation{}, ErrInvalidIdentityProof
	}
	hash, err := keyHash(oidcKey)
	if err != nil || hash != a.OIDCKeyHash {
		return Attestation{}, ErrInvalidIdentityProof
	}
	member, err := v.Directory.Member(ctx, a.SubjectIssuer, a.SubjectID)
	if err != nil || !slices.Contains(a.Roles, "deployer") || !slices.Contains(member.Roles, "deployer") || ctx.Err() != nil {
		return Attestation{}, ErrInvalidIdentityProof
	}
	now = time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	if !a.ExpiresAt.After(now) {
		return Attestation{}, ErrInvalidIdentityProof
	}
	return a, nil
}
func (v *AttestationVerifier) Verify(ctx context.Context, proof string) (Subject, error) {
	a, err := v.VerifyAttestation(ctx, proof)
	if err != nil {
		return Subject{}, err
	}
	return Subject{Issuer: a.SubjectIssuer, ID: a.SubjectID, Audience: []string{a.Audience}, IssuedAt: a.IssuedAt, ExpiresAt: a.ExpiresAt, Claims: map[string]any{"attestation_version": attestationVersion, "tenant_id": a.TenantID, "subject_type": a.SubjectType, "request_id": a.RequestID, "agent_id": a.AgentID, "delegation_ref": a.DelegationRef}}, nil
}
