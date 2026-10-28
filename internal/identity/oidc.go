package identity

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

var ErrInvalidIdentityProof = errors.New("INVALID_USER_IDENTITY_PROOF")

type OIDCKeyResolver interface {
	ResolveOIDCKey(context.Context, string, string) (crypto.PublicKey, error)
}

type StaticOIDCKeys map[string]crypto.PublicKey

func (s StaticOIDCKeys) ResolveOIDCKey(_ context.Context, issuer, keyID string) (crypto.PublicKey, error) {
	key, ok := s[issuer+"\x00"+keyID]
	if !ok {
		return nil, fmt.Errorf("unknown OIDC key")
	}
	return key, nil
}

type OIDCVerifier struct {
	Issuer    string
	Audience  string
	Keys      OIDCKeyResolver
	Now       func() time.Time
	ClockSkew time.Duration
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}
type jwtClaims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  audience `json:"aud"`
	ExpiresAt int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	IssuedAt  int64    `json:"iat"`
}
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*a = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (v *OIDCVerifier) Verify(ctx context.Context, token string) (Subject, error) {
	parts := strings.Split(token, ".")
	if ctx.Err() != nil || len(parts) != 3 || v.Issuer == "" || v.Audience == "" || v.Keys == nil {
		return Subject{}, ErrInvalidIdentityProof
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Subject{}, ErrInvalidIdentityProof
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Subject{}, ErrInvalidIdentityProof
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Subject{}, ErrInvalidIdentityProof
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.KeyID == "" || (header.Algorithm != "EdDSA" && header.Algorithm != "RS256") {
		return Subject{}, ErrInvalidIdentityProof
	}
	key, err := v.Keys.ResolveOIDCKey(ctx, v.Issuer, header.KeyID)
	if err != nil {
		return Subject{}, fmt.Errorf("%w: %v", ErrInvalidIdentityProof, err)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	if !verifyJWTSignature(header.Algorithm, key, signingInput, signature) {
		return Subject{}, ErrInvalidIdentityProof
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Subject{}, ErrInvalidIdentityProof
	}
	var allClaims map[string]any
	if err := json.Unmarshal(payload, &allClaims); err != nil {
		return Subject{}, ErrInvalidIdentityProof
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	if claims.Issuer != v.Issuer || claims.Subject == "" || !contains(claims.Audience, v.Audience) || claims.ExpiresAt == 0 || !time.Unix(claims.ExpiresAt, 0).After(now.Add(-v.ClockSkew)) {
		return Subject{}, ErrInvalidIdentityProof
	}
	if ctx.Err() != nil || (claims.NotBefore != 0 && time.Unix(claims.NotBefore, 0).After(now.Add(v.ClockSkew))) {
		return Subject{}, ErrInvalidIdentityProof
	}
	if claims.IssuedAt != 0 && time.Unix(claims.IssuedAt, 0).After(now.Add(v.ClockSkew)) {
		return Subject{}, ErrInvalidIdentityProof
	}
	return Subject{Issuer: claims.Issuer, ID: claims.Subject, Audience: append([]string(nil), claims.Audience...), ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), IssuedAt: time.Unix(claims.IssuedAt, 0).UTC(), Claims: allClaims}, nil
}

func verifyJWTSignature(algorithm string, key crypto.PublicKey, input, signature []byte) bool {
	switch algorithm {
	case "EdDSA":
		public, ok := key.(ed25519.PublicKey)
		return ok && ed25519.Verify(public, input, signature)
	case "RS256":
		public, ok := key.(*rsa.PublicKey)
		if !ok || public.N == nil || public.N.Cmp(big.NewInt(0)) <= 0 {
			return false
		}
		digest := sha256.Sum256(input)
		return rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature) == nil
	default:
		return false
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
