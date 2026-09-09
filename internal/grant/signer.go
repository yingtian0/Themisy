package grant

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"themisy/pkg/protocol"
)

const AlgorithmEd25519 = "Ed25519"

// Signer is the boundary implemented by production KMS/HSM integrations.
// Implementations receive only canonical bytes and never expose private key material.
type Signer interface {
	Sign(context.Context, []byte) (protocol.Signature, error)
}

// LoadDevelopmentSigner loads an explicitly provisioned development key. The
// file must contain a base64url/raw-base64 Ed25519 seed or private key and must
// not be accessible by group or other users.
func LoadDevelopmentSigner(path, keyID string) (*DevelopmentSigner, ed25519.PublicKey, error) {
	if path == "" || keyID == "" {
		return nil, nil, fmt.Errorf("development private key file and key id are required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat development grant key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, nil, fmt.Errorf("development grant key must not be group or world accessible")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read development grant key: %w", err)
	}
	key, err := decodeKey(strings.TrimSpace(string(encoded)))
	if err != nil {
		return nil, nil, err
	}
	var private ed25519.PrivateKey
	switch len(key) {
	case ed25519.SeedSize:
		private = ed25519.NewKeyFromSeed(key)
	case ed25519.PrivateKeySize:
		private = append(ed25519.PrivateKey(nil), key...)
	default:
		return nil, nil, fmt.Errorf("development grant key must be an Ed25519 seed or private key")
	}
	public := append(ed25519.PublicKey(nil), private.Public().(ed25519.PublicKey)...)
	return &DevelopmentSigner{keyID: keyID, key: private}, public, nil
}

func LoadEd25519PublicKey(path string) (ed25519.PublicKey, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Ed25519 public key: %w", err)
	}
	key, err := decodeKey(strings.TrimSpace(string(encoded)))
	if err != nil {
		return nil, err
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("Ed25519 public key must be %d bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(key), nil
}

func decodeKey(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("key must be base64 encoded")
}

type KMSClient interface {
	Sign(context.Context, string, []byte) ([]byte, error)
}

type KMSSigner struct {
	Client    KMSClient
	KeyID     string
	Algorithm string
}

func (s KMSSigner) Sign(ctx context.Context, payload []byte) (protocol.Signature, error) {
	if s.Client == nil || s.KeyID == "" || s.Algorithm == "" {
		return protocol.Signature{}, fmt.Errorf("KMS signer is not configured")
	}
	value, err := s.Client.Sign(ctx, s.KeyID, payload)
	if err != nil {
		return protocol.Signature{}, fmt.Errorf("KMS sign: %w", err)
	}
	return protocol.Signature{Algorithm: s.Algorithm, KeyID: s.KeyID, Value: base64.RawURLEncoding.EncodeToString(value)}, nil
}

// DevelopmentSigner is intentionally explicit and must not be selected as a
// production default. Its private key exists only in the local process.
type DevelopmentSigner struct {
	keyID string
	key   ed25519.PrivateKey
}

func NewDevelopmentSigner(keyID string) (*DevelopmentSigner, ed25519.PublicKey, error) {
	if keyID == "" {
		return nil, nil, fmt.Errorf("development key id is required")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return &DevelopmentSigner{keyID: keyID, key: private}, public, nil
}

func (s *DevelopmentSigner) Sign(_ context.Context, payload []byte) (protocol.Signature, error) {
	if len(s.key) != ed25519.PrivateKeySize {
		return protocol.Signature{}, fmt.Errorf("development signer is not initialized")
	}
	value := ed25519.Sign(s.key, payload)
	return protocol.Signature{Algorithm: AlgorithmEd25519, KeyID: s.keyID, Value: base64.RawURLEncoding.EncodeToString(value)}, nil
}

func SignActionGrant(ctx context.Context, signer Signer, actionGrant protocol.ActionGrant) (protocol.ActionGrant, error) {
	payload, err := protocol.CanonicalGrantPayload(actionGrant)
	if err != nil {
		return protocol.ActionGrant{}, err
	}
	signature, err := signer.Sign(ctx, payload)
	if err != nil {
		return protocol.ActionGrant{}, err
	}
	actionGrant.Signature = signature
	return actionGrant, nil
}
