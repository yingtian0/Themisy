package identity

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	defaultOIDCKeyCacheTTL = 5 * time.Minute
	maxOIDCKeyCacheTTL     = time.Hour
	minOIDCKeyCacheTTL     = time.Second
	maxOIDCMetadataBytes   = 256 << 10
	maxOIDCJWKSBytes       = 1 << 20
	unknownKeyRefreshGap   = 10 * time.Second
)

var ErrOIDCKeyUnavailable = errors.New("OIDC signing key unavailable")

// OIDCIssuerConfig is supplied only from trusted tenant configuration. The
// discovery URL and the issuer returned by discovery are pinned together.
type OIDCIssuerConfig struct {
	DiscoveryURL string
	CacheTTL     time.Duration
	HTTPClient   *http.Client
}

type oidcKeySet struct {
	keys      map[string]crypto.PublicKey
	expiresAt time.Time
}

type oidcIssuerState struct {
	mu                 sync.Mutex
	keySet             oidcKeySet
	lastRefreshAttempt time.Time
}

// OIDCJWKSResolver resolves only issuers explicitly present in trusted
// configuration. Metadata fetches are HTTPS-only, bounded, time-limited, and
// never follow redirects. Expired keys are not used as a fallback.
type OIDCJWKSResolver struct {
	configs map[string]OIDCIssuerConfig
	states  map[string]*oidcIssuerState
	Now     func() time.Time
}

func NewOIDCJWKSResolver(configs map[string]OIDCIssuerConfig) (*OIDCJWKSResolver, error) {
	if len(configs) == 0 {
		return nil, errors.New("at least one trusted OIDC issuer is required")
	}
	out := make(map[string]OIDCIssuerConfig, len(configs))
	states := make(map[string]*oidcIssuerState, len(configs))
	for issuer, config := range configs {
		if err := validateHTTPSURL(issuer); err != nil {
			return nil, fmt.Errorf("invalid OIDC issuer: %w", err)
		}
		if err := validateHTTPSURL(config.DiscoveryURL); err != nil {
			return nil, fmt.Errorf("invalid OIDC discovery URL: %w", err)
		}
		if config.CacheTTL == 0 {
			config.CacheTTL = defaultOIDCKeyCacheTTL
		}
		if config.CacheTTL < minOIDCKeyCacheTTL || config.CacheTTL > maxOIDCKeyCacheTTL {
			return nil, errors.New("OIDC key cache TTL is outside the allowed range")
		}
		config.HTTPClient = boundedOIDCHTTPClient(config.HTTPClient)
		out[issuer] = config
		states[issuer] = &oidcIssuerState{}
	}
	return &OIDCJWKSResolver{configs: out, states: states, Now: time.Now}, nil
}

func (r *OIDCJWKSResolver) ResolveOIDCKey(ctx context.Context, issuer, keyID string) (crypto.PublicKey, error) {
	if r == nil || keyID == "" {
		return nil, ErrOIDCKeyUnavailable
	}
	config, trusted := r.configs[issuer]
	state := r.states[issuer]
	if !trusted || state == nil {
		return nil, ErrOIDCKeyUnavailable
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	now := r.now()
	if now.Before(state.keySet.expiresAt) {
		if key, ok := state.keySet.keys[keyID]; ok {
			return key, nil
		}
	}
	if !state.lastRefreshAttempt.IsZero() && now.Sub(state.lastRefreshAttempt) < unknownKeyRefreshGap {
		return nil, ErrOIDCKeyUnavailable
	}
	state.lastRefreshAttempt = now
	keys, err := fetchOIDCKeySet(ctx, config, issuer)
	if err != nil {
		return nil, ErrOIDCKeyUnavailable
	}
	state.keySet = oidcKeySet{keys: keys, expiresAt: now.Add(config.CacheTTL)}
	if key, ok := keys[keyID]; ok {
		return key, nil
	}
	return nil, ErrOIDCKeyUnavailable
}

func (r *OIDCJWKSResolver) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func fetchOIDCKeySet(ctx context.Context, config OIDCIssuerConfig, expectedIssuer string) (map[string]crypto.PublicKey, error) {
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getOIDCJSON(ctx, config.HTTPClient, config.DiscoveryURL, maxOIDCMetadataBytes, &discovery); err != nil || discovery.Issuer != expectedIssuer || validateHTTPSURL(discovery.JWKSURI) != nil {
		return nil, ErrOIDCKeyUnavailable
	}
	var document struct {
		Keys []struct {
			KeyID     string   `json:"kid"`
			KeyType   string   `json:"kty"`
			Use       string   `json:"use"`
			Algorithm string   `json:"alg"`
			KeyOps    []string `json:"key_ops"`
			Modulus   string   `json:"n"`
			Exponent  string   `json:"e"`
			Curve     string   `json:"crv"`
			X         string   `json:"x"`
		} `json:"keys"`
	}
	if err := getOIDCJSON(ctx, config.HTTPClient, discovery.JWKSURI, maxOIDCJWKSBytes, &document); err != nil || len(document.Keys) == 0 {
		return nil, ErrOIDCKeyUnavailable
	}
	keys := make(map[string]crypto.PublicKey, len(document.Keys))
	for _, jwk := range document.Keys {
		if jwk.KeyID == "" {
			continue
		}
		if (jwk.Use != "" && jwk.Use != "sig") || !oidcKeyAllowsVerify(jwk.KeyOps) {
			continue
		}
		key, supported, err := parseOIDCPublicKey(jwk.KeyType, jwk.Algorithm, jwk.Curve, jwk.Modulus, jwk.Exponent, jwk.X)
		if err != nil {
			return nil, ErrOIDCKeyUnavailable
		}
		if !supported {
			continue
		}
		if _, exists := keys[jwk.KeyID]; exists {
			return nil, ErrOIDCKeyUnavailable
		}
		keys[jwk.KeyID] = key
	}
	if len(keys) == 0 {
		return nil, ErrOIDCKeyUnavailable
	}
	return keys, nil
}

func oidcKeyAllowsVerify(operations []string) bool {
	if len(operations) == 0 {
		return true
	}
	for _, operation := range operations {
		if operation == "verify" {
			return true
		}
	}
	return false
}

func parseOIDCPublicKey(keyType, algorithm, curve, modulus, exponent, x string) (crypto.PublicKey, bool, error) {
	switch keyType {
	case "RSA":
		if algorithm != "" && algorithm != "RS256" {
			return nil, false, nil
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(modulus)
		if err != nil || len(nBytes) < 256 || len(nBytes) > 1024 {
			return nil, false, ErrOIDCKeyUnavailable
		}
		n := new(big.Int).SetBytes(nBytes)
		if n.BitLen() < 2048 || n.BitLen() > 8192 {
			return nil, false, ErrOIDCKeyUnavailable
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(exponent)
		if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			return nil, false, ErrOIDCKeyUnavailable
		}
		e := 0
		for _, value := range eBytes {
			e = e<<8 | int(value)
		}
		if e < 3 || e%2 == 0 {
			return nil, false, ErrOIDCKeyUnavailable
		}
		return &rsa.PublicKey{N: n, E: e}, true, nil
	case "OKP":
		if curve != "Ed25519" || (algorithm != "" && algorithm != "EdDSA") {
			return nil, false, nil
		}
		public, err := base64.RawURLEncoding.DecodeString(x)
		if err != nil || len(public) != ed25519.PublicKeySize {
			return nil, false, ErrOIDCKeyUnavailable
		}
		return ed25519.PublicKey(public), true, nil
	default:
		return nil, false, nil
	}
}

func getOIDCJSON(ctx context.Context, client *http.Client, target string, limit int64, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ErrOIDCKeyUnavailable
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return ErrOIDCKeyUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrOIDCKeyUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit || json.Unmarshal(data, destination) != nil {
		return ErrOIDCKeyUnavailable
	}
	return nil
}

func validateHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("URL must be HTTPS without credentials, query or fragment")
	}
	return nil
}

func boundedOIDCHTTPClient(client *http.Client) *http.Client {
	copy := &http.Client{Timeout: 3 * time.Second}
	if client != nil {
		*copy = *client
		if copy.Timeout <= 0 || copy.Timeout > 10*time.Second {
			copy.Timeout = 3 * time.Second
		}
	}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return copy
}

var _ OIDCKeyResolver = (*OIDCJWKSResolver)(nil)
