package grant

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const maxGrantTrustBundleBytes = 64 << 10

// FileKeys resolves only keys from a fresh, Runner-local trust bundle. It does
// not cache keys, fetch trust over the network, or fall back to a prior bundle.
type FileKeys struct {
	Path   string
	Issuer string
	Tenant string
	Now    func() time.Time
}

type GrantTrustBundle struct {
	Version   string          `json:"version"`
	Issuer    string          `json:"issuer"`
	TenantID  string          `json:"tenant_id"`
	IssuedAt  time.Time       `json:"issued_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	Keys      []GrantTrustKey `json:"keys"`
}

type GrantTrustKey struct {
	KeyID     string    `json:"key_id"`
	PublicKey string    `json:"public_key"`
	Status    string    `json:"status"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

func (f FileKeys) Validate(ctx context.Context) error {
	_, err := f.ResolveGrantKey(ctx, f.Issuer, "")
	return err
}

func (f FileKeys) ResolveGrantKey(ctx context.Context, issuer, keyID string) (ed25519.PublicKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.Path == "" || f.Issuer == "" || f.Tenant == "" || issuer != f.Issuer {
		return nil, errors.New("grant trust bundle identity binding is not configured or does not match")
	}
	pathInfo, err := os.Lstat(f.Path)
	if err != nil {
		return nil, fmt.Errorf("grant trust bundle unavailable: %w", err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Size() > maxGrantTrustBundleBytes {
		return nil, errors.New("grant trust bundle must be a bounded regular file")
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return nil, fmt.Errorf("grant trust bundle unavailable: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat grant trust bundle: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxGrantTrustBundleBytes {
		return nil, errors.New("grant trust bundle must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxGrantTrustBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read grant trust bundle: %w", err)
	}
	if len(raw) > maxGrantTrustBundleBytes {
		return nil, errors.New("grant trust bundle exceeds size limit")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var bundle GrantTrustBundle
	if err := decoder.Decode(&bundle); err != nil {
		return nil, fmt.Errorf("decode grant trust bundle: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("grant trust bundle contains trailing data")
	}
	if bundle.Version != "themisy.grant-trust/v1" || bundle.Issuer != f.Issuer || bundle.TenantID != f.Tenant || len(bundle.Keys) == 0 || len(bundle.Keys) > 64 {
		return nil, errors.New("grant trust bundle version or binding is invalid")
	}
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now().UTC()
	}
	if bundle.IssuedAt.IsZero() || bundle.IssuedAt.After(now) || bundle.ExpiresAt.IsZero() || !bundle.ExpiresAt.After(now) {
		return nil, errors.New("grant trust bundle is not currently valid")
	}
	seenIDs := make(map[string]struct{}, len(bundle.Keys))
	seenKeys := make(map[string]struct{}, len(bundle.Keys))
	var resolved ed25519.PublicKey
	activeKeys := 0
	for _, entry := range bundle.Keys {
		if entry.KeyID == "" || entry.NotBefore.IsZero() || entry.NotAfter.IsZero() || !entry.NotAfter.After(entry.NotBefore) {
			return nil, errors.New("grant trust bundle contains an invalid key entry")
		}
		if _, ok := seenIDs[entry.KeyID]; ok {
			return nil, errors.New("grant trust bundle contains duplicate key IDs")
		}
		if entry.Status != "active" && entry.Status != "revoked" {
			return nil, errors.New("grant trust bundle contains an unknown key status")
		}
		seenIDs[entry.KeyID] = struct{}{}
		publicKey, err := base64.RawURLEncoding.DecodeString(entry.PublicKey)
		if err != nil || len(publicKey) != ed25519.PublicKeySize {
			return nil, errors.New("grant trust bundle contains an invalid Ed25519 key")
		}
		fingerprint := string(publicKey)
		if _, ok := seenKeys[fingerprint]; ok {
			return nil, errors.New("grant trust bundle aliases public keys across IDs")
		}
		seenKeys[fingerprint] = struct{}{}
		if entry.Status == "active" && !now.Before(entry.NotBefore) && now.Before(entry.NotAfter) {
			activeKeys++
		}
		if keyID != "" && entry.KeyID != keyID {
			continue
		}
		if keyID == "" {
			continue
		}
		if entry.Status != "active" || now.Before(entry.NotBefore) || !now.Before(entry.NotAfter) {
			return nil, errors.New("grant key is revoked or outside its validity window")
		}
		resolved = append(ed25519.PublicKey(nil), publicKey...)
	}
	if keyID == "" && activeKeys > 0 {
		return nil, nil
	}
	if keyID == "" {
		return nil, errors.New("grant trust bundle has no currently active key")
	}
	if resolved == nil {
		return nil, errors.New("unknown grant key ID")
	}
	return resolved, nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var readValue func(int) error
	readValue = func(depth int) error {
		if depth > 32 {
			return errors.New("grant trust bundle nesting limit exceeded")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		open, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch open {
		case '{':
			seen := make(map[string]struct{})
			allowed := map[string]struct{}{}
			switch depth {
			case 0:
				allowed = map[string]struct{}{"version": {}, "issuer": {}, "tenant_id": {}, "issued_at": {}, "expires_at": {}, "keys": {}}
			case 2:
				allowed = map[string]struct{}{"key_id": {}, "public_key": {}, "status": {}, "not_before": {}, "not_after": {}}
			default:
				return errors.New("grant trust bundle contains an unexpected object")
			}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("grant trust bundle has an invalid object key")
				}
				if _, exists := allowed[key]; !exists {
					return fmt.Errorf("grant trust bundle contains unknown JSON key %q", key)
				}
				foldedKey := strings.ToLower(key)
				if _, exists := seen[foldedKey]; exists {
					return fmt.Errorf("grant trust bundle contains duplicate JSON key %q", key)
				}
				seen[foldedKey] = struct{}{}
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("grant trust bundle has an invalid JSON delimiter")
		}
	}
	if err := readValue(0); err != nil {
		return fmt.Errorf("parse grant trust bundle: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("grant trust bundle contains trailing data")
	}
	return nil
}
