package identity

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

type AttestationRequest struct {
	RequestID     string `json:"request_id"`
	AgentID       string `json:"agent_id"`
	DelegationRef string `json:"delegation_ref"`
}
type Attester interface {
	Exchange(context.Context, string, AttestationRequest) (string, error)
}

// RemoteAttester never follows redirects and sends bearer authorization only to
// the operator-configured HTTPS mTLS identity authority. It holds no signer.
type RemoteAttester struct {
	URL  string
	HTTP *http.Client
}

func NewRemoteAttester(endpoint, caFile, certFile, keyFile string) (*RemoteAttester, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid identity attestation endpoint")
	}
	info, err := os.Stat(keyFile)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("identity exchange mTLS key must be protected")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("identity exchange CA unavailable")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{cert}}
	return &RemoteAttester{URL: endpoint, HTTP: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (r *RemoteAttester) Exchange(ctx context.Context, token string, input AttestationRequest) (string, error) {
	if r == nil || r.HTTP == nil || token == "" || len(token) > 16384 || ctx.Err() != nil {
		return "", ErrInvalidIdentityProof
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", ErrInvalidIdentityProof
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return "", ErrInvalidIdentityProof
	}
	req, err := http.NewRequestWithContext(ctx, "POST", r.URL, bytes.NewReader(raw))
	if err != nil {
		return "", ErrInvalidIdentityProof
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := *r.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 5 * time.Second
	response, err := client.Do(req)
	if err != nil {
		return "", ErrInvalidIdentityProof
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", ErrInvalidIdentityProof
	}
	var output struct {
		Proof string `json:"proof"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16385))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&output) != nil || decoder.Decode(new(any)) != io.EOF || len(output.Proof) > 8192 || output.Proof == "" || ctx.Err() != nil {
		return "", ErrInvalidIdentityProof
	}
	return output.Proof, nil
}
