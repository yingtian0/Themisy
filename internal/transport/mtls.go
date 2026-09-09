package transport

import (
	"errors"
	"net/http"
	"strings"
)

// MTLSRunnerAuthenticator binds a verified certificate URI SAN (normally a
// SPIFFE ID) to server-owned tenant, Runner group, and Runner ID values.
type MTLSRunnerAuthenticator map[string]RunnerIdentity

func (a MTLSRunnerAuthenticator) Authenticate(request *http.Request) (RunnerIdentity, error) {
	if request.TLS == nil || len(request.TLS.VerifiedChains) == 0 || len(request.TLS.VerifiedChains[0]) == 0 {
		return RunnerIdentity{}, errors.New("verified Runner client certificate is required")
	}
	leaf := request.TLS.VerifiedChains[0][0]
	for _, uri := range leaf.URIs {
		identity, ok := a[uri.String()]
		if !ok || identity.RunnerID == "" || identity.TenantID == "" || identity.RunnerGroup == "" {
			continue
		}
		claimed := strings.TrimSpace(request.Header.Get(runnerIDHeader))
		if claimed != "" && claimed != identity.RunnerID {
			return RunnerIdentity{}, errors.New("certificate identity does not match Runner ID")
		}
		return identity, nil
	}
	return RunnerIdentity{}, errors.New("Runner workload identity is not registered")
}

type HybridRunnerAuthenticator struct {
	MTLS   MTLSRunnerAuthenticator
	Bearer StaticRunnerAuthenticator
}

func (a HybridRunnerAuthenticator) Authenticate(request *http.Request) (RunnerIdentity, error) {
	if request.TLS != nil && len(request.TLS.PeerCertificates) > 0 {
		return a.MTLS.Authenticate(request)
	}
	return a.Bearer.Authenticate(request)
}
