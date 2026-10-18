# OIDC JWKS resolver

`internal/identity.OIDCJWKSResolver` resolves signing keys only for issuers
provided by trusted application configuration. It is a key-resolution
component; it does not implement tenant discovery, role mapping, delegation
issuance/revocation, or production HTTP header authentication. The current
server composition is not switched to this resolver by this change.

Each configured issuer needs its exact OIDC issuer URL and an HTTPS discovery
URL. Discovery's returned `issuer` must match exactly, and `jwks_uri` must also
use HTTPS. The resolver rejects URL credentials, query strings, fragments,
HTTP responses other than 200, and redirects. Requests have a three-second
timeout and bounded response sizes. Trust anchors and URLs must come from
operator configuration, never a request, token claim, or arbitrary proxy
header.

The default cache lifetime is five minutes. Configured lifetimes must be between
one second and one hour. Before expiry, known keys are served from cache. An
unknown `kid` triggers at most one refresh per issuer every ten seconds, which
permits rotation while limiting refresh traffic from invalid tokens. After
expiry, a fresh discovery/JWKS fetch is required; unavailable metadata never
falls back to an expired key. Supported signing keys are RSA at least 2048 bits
for `RS256`, and OKP/Ed25519 for `EdDSA`. Encryption-only keys, other key
algorithms, and unsupported key types are ignored. Duplicate usable key IDs or
malformed supported keys invalidate the response.

The existing `OIDCVerifier` still checks signature, exact issuer, audience,
subject, and expiry after resolving a key. Each tenant must create that verifier
with its own trusted issuer and audience settings. This resolver does not infer
tenant trust from `iss`, and a key response alone is not sufficient to authorize
a subject.

The resolver's tests use local TLS test servers and synthetic keys. They do not
demonstrate integration with an external identity provider, administrator
configuration, organization roles, current delegation revocation, or production
service composition.
