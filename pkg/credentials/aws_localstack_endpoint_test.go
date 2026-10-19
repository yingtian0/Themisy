package credentials

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Test-only endpoints must be explicit, literal loopback HTTP addresses. There
// is no AWS default endpoint, proxy, metadata credential chain or redirect path.
func localStackEndpoint(raw string) (string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid LocalStack endpoint")
	}
	port, err := strconv.Atoi(endpoint.Port())
	ip := net.ParseIP(endpoint.Hostname())
	if err != nil || port < 1 || port > 65535 || ip == nil || !ip.IsLoopback() || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.RawPath != "" || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", fmt.Errorf("LocalStack endpoint must be an explicit loopback HTTP address and port")
	}
	return strings.TrimRight(endpoint.String(), "/"), nil
}

func TestLocalStackEndpointCannotSelectRealAWS(t *testing.T) {
	for _, endpoint := range []string{"", "https://sts.amazonaws.com", "http://sts.amazonaws.com:4566", "http://localhost:4566", "http://192.0.2.1:4566", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user:secret@127.0.0.1:4566", "http://127.0.0.1:4566/other", "http://127.0.0.1:4566/%2F", "http://127.0.0.1:4566?target=aws", "http://127.0.0.1:4566/#x"} {
		if _, err := localStackEndpoint(endpoint); err == nil {
			t.Errorf("accepted non-fixture endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:4566", "http://127.0.0.1:4566/", "http://[::1]:4566"} {
		if _, err := localStackEndpoint(endpoint); err != nil {
			t.Errorf("rejected loopback endpoint: %v", err)
		}
	}
}
