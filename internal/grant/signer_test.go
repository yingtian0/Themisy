package grant

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDevelopmentSignerRequiresPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grant.key")
	seed := make([]byte, ed25519.SeedSize)
	if err := os.WriteFile(path, []byte(base64.RawURLEncoding.EncodeToString(seed)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadDevelopmentSigner(path, "test-key"); err == nil {
		t.Fatal("expected group-readable development key to be rejected")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	signer, publicKey, err := LoadDevelopmentSigner(path, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if signer == nil || len(publicKey) != ed25519.PublicKeySize {
		t.Fatalf("signer=%v public key bytes=%d", signer, len(publicKey))
	}
}
