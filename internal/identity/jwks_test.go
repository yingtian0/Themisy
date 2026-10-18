package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOIDCJWKSResolverCachesRotatesAndFailsClosedAfterExpiry(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotatedPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.RWMutex
	keys := []map[string]any{ed25519JWK("idp-1", public)}
	var discoveryRequests, jwksRequests atomic.Int32
	var failJWKS atomic.Bool
	issuer := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			discoveryRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
		case "/keys":
			jwksRequests.Add(1)
			if failJWKS.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			mu.RLock()
			defer mu.RUnlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	resolver, err := NewOIDCJWKSResolver(map[string]OIDCIssuerConfig{issuer: {DiscoveryURL: issuer + "/.well-known/openid-configuration", CacheTTL: time.Minute, HTTPClient: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	resolver.Now = func() time.Time { return now }
	key, err := resolver.ResolveOIDCKey(context.Background(), issuer, "idp-1")
	if err != nil || string(key.(ed25519.PublicKey)) != string(public) {
		t.Fatalf("resolved key=%T err=%v", key, err)
	}
	verifier := &OIDCVerifier{Issuer: issuer, Audience: "themisy", Keys: resolver, Now: func() time.Time { return now }}
	token := signJWT(t, private, map[string]any{"iss": issuer, "sub": "user-1", "aud": "themisy", "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix()})
	if subject, err := verifier.Verify(context.Background(), token); err != nil || subject.ID != "user-1" {
		t.Fatalf("remote-key token subject=%#v err=%v", subject, err)
	}
	if _, err := resolver.ResolveOIDCKey(context.Background(), issuer, "idp-1"); err != nil {
		t.Fatal(err)
	}
	if got := discoveryRequests.Load(); got != 1 {
		t.Fatalf("cache miss fetched discovery %d times, want 1", got)
	}

	mu.Lock()
	keys = append(keys, ed25519JWK("idp-2", rotatedPublic))
	mu.Unlock()
	now = now.Add(unknownKeyRefreshGap + time.Second)
	if key, err := resolver.ResolveOIDCKey(context.Background(), issuer, "idp-2"); err != nil || string(key.(ed25519.PublicKey)) != string(rotatedPublic) {
		t.Fatalf("rotated key=%T err=%v", key, err)
	}
	if discoveryRequests.Load() != 2 || jwksRequests.Load() != 2 {
		t.Fatalf("rotation refresh counts discovery=%d jwks=%d", discoveryRequests.Load(), jwksRequests.Load())
	}

	now = now.Add(time.Minute + time.Second)
	failJWKS.Store(true)
	if _, err := resolver.ResolveOIDCKey(context.Background(), issuer, "idp-1"); !errors.Is(err, ErrOIDCKeyUnavailable) {
		t.Fatalf("expired cached key was used during refresh failure: %v", err)
	}
}

func TestOIDCJWKSResolverRejectsUntrustedOrAmbiguousMetadata(t *testing.T) {
	if _, err := NewOIDCJWKSResolver(map[string]OIDCIssuerConfig{"http://identity.example": {DiscoveryURL: "http://identity.example/.well-known/openid-configuration"}}); err == nil {
		t.Fatal("HTTP issuer accepted")
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("issuer-mismatch", func(t *testing.T) {
		var issuer string
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/openid-configuration" {
				_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "https://other.example", "jwks_uri": issuer + "/keys"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{ed25519JWK("key", public)}})
		}))
		defer server.Close()
		issuer = server.URL
		resolver, err := NewOIDCJWKSResolver(map[string]OIDCIssuerConfig{issuer: {DiscoveryURL: issuer + "/.well-known/openid-configuration", HTTPClient: server.Client()}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.ResolveOIDCKey(context.Background(), issuer, "key"); !errors.Is(err, ErrOIDCKeyUnavailable) {
			t.Fatalf("issuer mismatch accepted: %v", err)
		}
	})
	t.Run("duplicate-kid", func(t *testing.T) {
		var issuer string
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/openid-configuration" {
				_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{ed25519JWK("duplicate", public), ed25519JWK("duplicate", public)}})
		}))
		defer server.Close()
		issuer = server.URL
		resolver, err := NewOIDCJWKSResolver(map[string]OIDCIssuerConfig{issuer: {DiscoveryURL: issuer + "/.well-known/openid-configuration", HTTPClient: server.Client()}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.ResolveOIDCKey(context.Background(), issuer, "duplicate"); !errors.Is(err, ErrOIDCKeyUnavailable) {
			t.Fatalf("duplicate key id accepted: %v", err)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var targetRequests atomic.Int32
		target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			targetRequests.Add(1)
			_, _ = w.Write([]byte(`{"keys":[]}`))
		}))
		defer target.Close()
		var issuer string
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/openid-configuration" {
				_, _ = w.Write([]byte(`{"issuer":"` + issuer + `","jwks_uri":"` + issuer + `/keys"}`))
				return
			}
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer server.Close()
		issuer = server.URL
		resolver, err := NewOIDCJWKSResolver(map[string]OIDCIssuerConfig{issuer: {DiscoveryURL: issuer + "/.well-known/openid-configuration", HTTPClient: server.Client()}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.ResolveOIDCKey(context.Background(), issuer, "key"); !errors.Is(err, ErrOIDCKeyUnavailable) {
			t.Fatalf("redirect response accepted: %v", err)
		}
		if targetRequests.Load() != 0 {
			t.Fatal("OIDC resolver followed a metadata redirect")
		}
	})
}

func TestOIDCJWKSResolverConfigurationBoundsCacheTTL(t *testing.T) {
	issuer := "https://id.example/tenant"
	discovery := "https://id.example/tenant/.well-known/openid-configuration"
	resolver, err := NewOIDCJWKSResolver(map[string]OIDCIssuerConfig{issuer: {DiscoveryURL: discovery}})
	if err != nil || resolver.configs[issuer].CacheTTL != defaultOIDCKeyCacheTTL {
		t.Fatalf("default cache config=%#v err=%v", resolver, err)
	}
	for _, ttl := range []time.Duration{-time.Second, minOIDCKeyCacheTTL / 2, maxOIDCKeyCacheTTL + 1} {
		if _, err := NewOIDCJWKSResolver(map[string]OIDCIssuerConfig{issuer: {DiscoveryURL: discovery, CacheTTL: ttl}}); err == nil {
			t.Errorf("cache TTL %s accepted", ttl)
		}
	}
}

func TestParseOIDCRSAJWKRequiresStrongKeyAndValidExponent(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, supported, err := parseOIDCPublicKey("RSA", "RS256", "", base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()), "")
	if err != nil || !supported {
		t.Fatalf("valid RSA key rejected: supported=%v err=%v", supported, err)
	}
	resolved := public.(*rsa.PublicKey)
	if resolved.N.Cmp(key.N) != 0 || resolved.E != key.E {
		t.Fatal("resolved RSA JWK did not preserve public key")
	}
	if _, supported, err := parseOIDCPublicKey("RSA", "RS384", "", "", "", ""); err != nil || supported {
		t.Fatalf("unsupported signing algorithm supported=%v err=%v", supported, err)
	}
}

func ed25519JWK(keyID string, public ed25519.PublicKey) map[string]any {
	return map[string]any{"kid": keyID, "kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "key_ops": []string{"verify"}, "x": base64.RawURLEncoding.EncodeToString(public)}
}
