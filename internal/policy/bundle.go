package policy

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"themisy/internal/grant"
	"themisy/internal/policymodel"
	"themisy/pkg/protocol"
)

type Layer = policymodel.Layer

const (
	LayerPlatform    = policymodel.LayerPlatform
	LayerEnvironment = policymodel.LayerEnvironment
	LayerTeam        = policymodel.LayerTeam
	LayerService     = policymodel.LayerService
)

type Module = policymodel.Module
type Bundle = policymodel.Bundle

type BundleKeyResolver interface {
	ResolvePolicyKey(context.Context, string, string) (ed25519.PublicKey, error)
}
type StaticBundleKeys map[string]ed25519.PublicKey

func (s StaticBundleKeys) ResolvePolicyKey(_ context.Context, issuer, keyID string) (ed25519.PublicKey, error) {
	key, ok := s[issuer+"\x00"+keyID]
	if !ok {
		return nil, fmt.Errorf("unknown policy bundle key")
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

var ErrInvalidBundle = errors.New("INVALID_POLICY_BUNDLE")

func CalculateBundleHash(bundle Bundle) (string, error) {
	copy := normalizedBundle(bundle)
	copy.Hash, copy.Signature = "", protocol.Signature{}
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func CanonicalBundlePayload(bundle Bundle) ([]byte, error) {
	copy := normalizedBundle(bundle)
	copy.Signature = protocol.Signature{}
	if copy.Version == "" || copy.Issuer == "" || copy.Hash == "" || copy.IssuedAt.IsZero() || !copy.ExpiresAt.After(copy.IssuedAt) || len(copy.Modules) == 0 {
		return nil, ErrInvalidBundle
	}
	return json.Marshal(copy)
}

const BundleSigningVersion = "themisy.policy-signature/v2"

func bundleSigningPayload(bundle Bundle) ([]byte, error) {
	raw, err := CanonicalBundlePayload(bundle)
	if err != nil {
		return nil, err
	}
	if bundle.SigningVersion == "" {
		return raw, nil
	}
	if bundle.SigningVersion != BundleSigningVersion {
		return nil, ErrInvalidBundle
	}
	sum := sha256.Sum256(raw)
	return []byte(BundleSigningVersion + "\x00" + hex.EncodeToString(sum[:])), nil
}
func SignBundle(ctx context.Context, signer grant.Signer, bundle Bundle) (Bundle, error) {
	if signer == nil || ctx.Err() != nil {
		return Bundle{}, ErrInvalidBundle
	}
	bundle.SigningVersion = BundleSigningVersion
	hash, err := CalculateBundleHash(bundle)
	if err != nil {
		return Bundle{}, err
	}
	bundle.Hash = hash
	payload, err := bundleSigningPayload(bundle)
	if err != nil {
		return Bundle{}, err
	}
	bundle.Signature, err = signer.Sign(ctx, payload)
	return bundle, err
}

func VerifyBundle(ctx context.Context, bundle Bundle, issuer string, keys BundleKeyResolver, now time.Time) error {
	if ctx.Err() != nil || bundle.Issuer != issuer || bundle.Signature.Algorithm != grant.AlgorithmEd25519 || keys == nil || !containsString(bundle.Compatibility, InputVersionV1Alpha1) || bundle.IssuedAt.After(now.UTC()) || !bundle.ExpiresAt.After(now.UTC()) || !validLayers(bundle.Modules) {
		return ErrInvalidBundle
	}
	expected, err := CalculateBundleHash(bundle)
	if err != nil || expected != bundle.Hash {
		return ErrInvalidBundle
	}
	payload, err := bundleSigningPayload(bundle)
	if err != nil {
		return ErrInvalidBundle
	}
	key, err := keys.ResolvePolicyKey(ctx, issuer, bundle.Signature.KeyID)
	if err != nil {
		return ErrInvalidBundle
	}
	signature, err := base64.RawURLEncoding.DecodeString(bundle.Signature.Value)
	if err != nil || len(key) != ed25519.PublicKeySize || ctx.Err() != nil || !ed25519.Verify(key, payload, signature) {
		return ErrInvalidBundle
	}
	return nil
}

func validLayers(modules []Module) bool {
	hasPlatform := false
	seen := map[string]bool{}
	for _, module := range modules {
		switch module.Layer {
		case LayerPlatform:
			hasPlatform = true
		case LayerEnvironment, LayerTeam, LayerService:
		default:
			return false
		}
		key := string(module.Layer) + "/" + module.Name
		if seen[key] {
			return false
		}
		seen[key] = true
		if module.Name == "" || module.Source == "" {
			return false
		}
	}
	return hasPlatform
}

func normalizedBundle(bundle Bundle) Bundle {
	bundle.Compatibility = append([]string(nil), bundle.Compatibility...)
	bundle.Modules = append([]Module(nil), bundle.Modules...)
	sort.Strings(bundle.Compatibility)
	sort.Slice(bundle.Modules, func(i, j int) bool {
		if bundle.Modules[i].Layer == bundle.Modules[j].Layer {
			return bundle.Modules[i].Name < bundle.Modules[j].Name
		}
		return bundle.Modules[i].Layer < bundle.Modules[j].Layer
	})
	return bundle
}
