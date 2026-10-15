package verification

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

func TestServiceObtainsCredentialLocallyAndValidatesEvidence(t *testing.T) {
	now := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	request := adapter.VerificationRequest{Target: adapter.Target{Service: "identity", Environment: "production"}, Deployment: adapter.Deployment{ExternalExecutionID: "run-1"}, Window: adapter.ObservationWindow{From: now.Add(-time.Minute), To: now}}
	evidence := adapter.Evidence{Status: adapter.VerificationPass, ReasonCode: "THRESHOLD_MET", Source: "datadog", QueryHash: digest('a'), Window: request.Window, ObservedAt: now, ObservedValue: 0.2, Threshold: 1, AdapterVersion: "datadog/v1"}
	evidence.EvidenceHash, _ = adapter.EvidenceHash(evidence)
	secrets := []string{"private-api-key", "private-app-key"}
	broker := &recordingBroker{credential: credentials.New(map[string][]byte{"api_key": []byte(secrets[0]), "app_key": []byte(secrets[1])}, now.Add(time.Hour))}
	verifier := &staticVerifier{evidence: evidence}
	service := &Service{TenantID: "tenant-1", Broker: broker, Adapter: verifier}
	actual, err := service.Verify(context.Background(), request)
	if err != nil || actual.Status != adapter.VerificationPass || broker.calls != 1 || verifier.received.Value("api_key") == nil {
		t.Fatalf("evidence=%#v err=%v broker=%d", actual, err, broker.calls)
	}
	if broker.request.Provider != adapter.ProviderDatadog || broker.request.Service != "identity" {
		t.Fatalf("credential request=%#v", broker.request)
	}
	encoded, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("credential value leaked into verification evidence: %s", encoded)
		}
	}
}

func TestServiceRejectsTamperedEvidence(t *testing.T) {
	now := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	evidence := adapter.Evidence{Status: adapter.VerificationPass, ReasonCode: "PASS", Source: "datadog", QueryHash: digest('a'), Window: adapter.ObservationWindow{From: now.Add(-time.Minute), To: now}, ObservedAt: now, AdapterVersion: "datadog/v1", EvidenceHash: digest('b')}
	service := &Service{TenantID: "tenant-1", Broker: &recordingBroker{credential: credentials.New(map[string][]byte{"key": []byte("secret")}, now.Add(time.Hour))}, Adapter: &staticVerifier{evidence: evidence}}
	if _, err := service.Verify(context.Background(), adapter.VerificationRequest{Target: adapter.Target{Service: "identity", Environment: "staging"}}); err == nil {
		t.Fatal("tampered evidence accepted")
	}
}

type recordingBroker struct {
	credential credentials.Credential
	request    credentials.Request
	calls      int
}

func (b *recordingBroker) Acquire(_ context.Context, request credentials.Request) (credentials.Credential, error) {
	b.calls++
	b.request = request
	return b.credential.Clone(), nil
}

type staticVerifier struct {
	evidence adapter.Evidence
	received credentials.Credential
	err      error
}

func (*staticVerifier) Name() string    { return "static" }
func (*staticVerifier) Version() string { return "static/v1" }
func (v *staticVerifier) Verify(_ context.Context, _ adapter.VerificationRequest, credential credentials.Credential) (adapter.Evidence, error) {
	v.received = credential.Clone()
	if v.err != nil {
		return adapter.Evidence{}, v.err
	}
	return v.evidence, nil
}
func digest(value byte) string {
	result := make([]byte, 64)
	for index := range result {
		result[index] = value
	}
	return "sha256:" + string(result)
}
