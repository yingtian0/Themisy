// identityattestor is deployed under independent organization identity authority.
// Its workload may sign identity receipts, never Control Plane Action Grants.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"go.yaml.in/yaml/v3"
	"net/http"
	"net/url"
	"os"
	"strings"
	"themisy/internal/api"
	"themisy/internal/config"
	"themisy/internal/grant"
	"themisy/internal/identity"
	postgresstore "themisy/internal/store/postgres"
	"time"
)

type settings struct {
	Mode           string              `yaml:"mode"`
	HTTP           config.HTTP         `yaml:"http"`
	ClientCAFile   string              `yaml:"client_ca_file"`
	ClientIdentity string              `yaml:"client_identity"`
	Issuer         string              `yaml:"issuer"`
	Signing        config.GrantSigning `yaml:"signing"`
	Database       config.Database     `yaml:"database"`
	Identity       config.Identity     `yaml:"identity"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "", "independent identity authority configuration")
	flag.Parse()
	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var c settings
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return err
	}
	if value := os.Getenv("THEMISY_IDENTITY_DATABASE_URL"); value != "" {
		c.Database.URL = value
	}
	uri, err := url.Parse(c.ClientIdentity)
	if err != nil || uri.Scheme != "spiffe" || uri.Host == "" || uri.Path == "" || c.Database.URL == "" || c.Issuer == "" || c.Issuer == c.Identity.DirectoryIssuer || c.Identity.DirectoryMinimumRevision < 1 || c.Identity.TenantID == "" || c.Identity.AttestationTrustBundleFile == "" {
		return fmt.Errorf("independent identity authority configuration incomplete")
	}
	if c.Mode != "production" && c.Mode != "development" {
		return fmt.Errorf("identity authority mode must be explicit")
	}
	if c.Mode == "production" && c.Signing.Provider != "aws-kms" {
		return fmt.Errorf("production identity authority requires independent KMS signing")
	}
	tlsConfig, err := serverTLS(c)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if c.Database.AutoMigrate {
		if err := postgresstore.Migrate(c.Database.URL, false); err != nil {
			return err
		}
	}
	st, err := postgresstore.New(ctx, c.Database.URL)
	if err != nil {
		return err
	}
	defer st.Close()
	ttl, err := time.ParseDuration(c.Identity.CacheTTL)
	if err != nil {
		return err
	}
	oidcKeys, err := identity.NewOIDCJWKSResolver(map[string]identity.OIDCIssuerConfig{c.Identity.Issuer: {DiscoveryURL: c.Identity.DiscoveryURL, CacheTTL: ttl}})
	if err != nil {
		return err
	}
	oidc := identity.OIDCVerifier{Issuer: c.Identity.Issuer, Audience: c.Identity.Audience, Keys: oidcKeys}
	directory := &identity.FileDirectory{Path: c.Identity.DirectoryFile, Issuer: c.Identity.DirectoryIssuer, TenantID: c.Identity.TenantID, MinimumRevision: c.Identity.DirectoryMinimumRevision, Keys: grant.FileKeys{Path: c.Identity.DirectoryTrustBundleFile, Issuer: c.Identity.DirectoryIssuer, Tenant: c.Identity.TenantID}, Ledger: st}
	if _, err := directory.Read(ctx); err != nil {
		return err
	}
	proofKeys := grant.FileKeys{Path: c.Identity.AttestationTrustBundleFile, Issuer: c.Issuer, Tenant: c.Identity.TenantID}
	if _, err := proofKeys.ResolveGrantKey(ctx, c.Issuer, c.Signing.KeyID); err != nil {
		return err
	}
	var signer grant.Signer
	switch c.Signing.Provider {
	case "aws-kms":
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.Signing.AWSRegion))
		if err != nil {
			return err
		}
		signer, err = grant.NewAWSKMSSigner(kms.NewFromConfig(cfg), c.Signing.KeyID)
		if err != nil {
			return err
		}
	case "development":
		signer, _, err = grant.LoadDevelopmentSigner(c.Signing.PrivateKeyFile, c.Signing.KeyID)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported identity signer")
	}
	audited, err := api.NewAuditedOIDCIdentityResolver(api.OIDCPrincipalConfig{TenantID: c.Identity.TenantID, TenantClaim: c.Identity.TenantClaim, RoleClaim: c.Identity.RoleClaim, SubjectTypeClaim: c.Identity.SubjectTypeClaim, RoleMappings: c.Identity.RoleMappings, Verifier: oidc}, st)
	if err != nil {
		return err
	}
	resolver := &api.ProductionIdentityResolver{OIDC: audited, Directory: directory, Audit: st}
	attestor := &api.IdentityAttestor{Signer: signer, KeyID: c.Signing.KeyID, Audit: st, Verifier: &identity.AttestationVerifier{Issuer: c.Issuer, TenantID: c.Identity.TenantID, Audience: c.Identity.Audience, Keys: proofKeys, OIDC: &oidc, Directory: directory}}
	routes := http.NewServeMux()
	routes.Handle("POST /v1/identity/attest", api.Authenticate(attestor, resolver, nil))
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client identity unavailable", 403)
			return
		}
		matched := false
		for _, id := range r.TLS.PeerCertificates[0].URIs {
			matched = matched || id.String() == c.ClientIdentity
		}
		if !matched {
			http.Error(w, "client identity unavailable", 403)
			return
		}
		routes.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: c.HTTP.Address, Handler: protected, TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	return server.ListenAndServeTLS(c.HTTP.TLSCertFile, c.HTTP.TLSKeyFile)
}
func serverTLS(c settings) (*tls.Config, error) {
	info, err := os.Stat(c.HTTP.TLSKeyFile)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("identity server TLS key must be protected")
	}
	raw, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("identity client CA unavailable")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}, nil
}
