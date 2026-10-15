package grant

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"themisy/pkg/protocol"
)

var (
	ErrInvalidProtocol  = errors.New("INVALID_PROTOCOL_VERSION")
	ErrInvalidSignature = errors.New("INVALID_GRANT_SIGNATURE")
	ErrInvalidIssuer    = errors.New("INVALID_GRANT_ISSUER")
	ErrWrongAudience    = errors.New("WRONG_RUNNER_AUDIENCE")
	ErrWrongTenant      = errors.New("WRONG_TENANT")
	ErrExpired          = errors.New("EXPIRED_GRANT")
)

type KeyResolver interface {
	ResolveGrantKey(context.Context, string, string) (ed25519.PublicKey, error)
}

type StaticKeys map[string]ed25519.PublicKey

func (s StaticKeys) ResolveGrantKey(_ context.Context, issuer, keyID string) (ed25519.PublicKey, error) {
	key, ok := s[issuer+"\x00"+keyID]
	if !ok {
		return nil, fmt.Errorf("unknown grant key")
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

type Verifier struct {
	Issuer      string
	RunnerGroup string
	TenantID    string
	Keys        KeyResolver
	Now         func() time.Time
	ClockSkew   time.Duration
}

func (v *Verifier) Verify(ctx context.Context, actionGrant protocol.ActionGrant) error {
	if err := protocol.ValidateVersion(actionGrant.ProtocolVersion); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidProtocol, err)
	}
	payload, err := protocol.CanonicalGrantPayload(actionGrant)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	if actionGrant.Signature.Algorithm != AlgorithmEd25519 || actionGrant.Signature.KeyID == "" || actionGrant.Signature.Value == "" {
		return ErrInvalidSignature
	}
	if actionGrant.Issuer != v.Issuer {
		return ErrInvalidIssuer
	}
	if actionGrant.RunnerGroup != v.RunnerGroup {
		return ErrWrongAudience
	}
	if v.TenantID == "" || actionGrant.TenantID != v.TenantID {
		return ErrWrongTenant
	}
	if v.Keys == nil {
		return fmt.Errorf("%w: key resolver unavailable", ErrInvalidSignature)
	}
	publicKey, err := v.Keys.ResolveGrantKey(ctx, actionGrant.Issuer, actionGrant.Signature.KeyID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	value, err := base64.RawURLEncoding.DecodeString(actionGrant.Signature.Value)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, payload, value) {
		return ErrInvalidSignature
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	if actionGrant.IssuedAt.After(now.Add(v.ClockSkew)) || !actionGrant.ExpiresAt.After(now.Add(-v.ClockSkew)) {
		return ErrExpired
	}
	return nil
}
