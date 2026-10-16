package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/internal/policy"
	runnercore "themisy/internal/runner"
	"themisy/internal/store"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
)

func TestMTLSAuthenticatorBindsWorkloadIdentityToRunner(t *testing.T) {
	workloadID, err := url.Parse("spiffe://example.internal/themisy/runner-1")
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{URIs: []*url.URL{workloadID}}
	authenticator := MTLSRunnerAuthenticator{workloadID.String(): {RunnerID: "runner-1", TenantID: "tenant-1", RunnerGroup: "prod-jp"}}
	request := httptest.NewRequest(http.MethodGet, "/v1/runner/grants:next", nil)
	request.Header.Set(runnerIDHeader, "runner-1")
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}, VerifiedChains: [][]*x509.Certificate{{certificate}}}
	identity, err := authenticator.Authenticate(request)
	if err != nil || identity.TenantID != "tenant-1" || identity.RunnerGroup != "prod-jp" {
		t.Fatalf("identity=%#v err=%v", identity, err)
	}
	request.Header.Set(runnerIDHeader, "other-runner")
	if _, err := authenticator.Authenticate(request); err == nil {
		t.Fatal("certificate was accepted for a different Runner ID")
	}
	hybrid := HybridRunnerAuthenticator{MTLS: authenticator, Bearer: StaticRunnerAuthenticator{"runner-1": {TenantID: "tenant-1", RunnerGroup: "prod-jp", Token: "secret"}}}
	request.Header.Set(runnerIDHeader, "runner-1")
	request.Header.Set("Authorization", "Bearer secret")
	request.TLS.VerifiedChains = nil
	if _, err := hybrid.Authenticate(request); err == nil {
		t.Fatal("unverified client certificate downgraded to bearer authentication")
	}
}

func TestRunnerTransportRequiresVerifiedMTLSIdentity(t *testing.T) {
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Themisy test CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(ca)
	issueLeaf := func(serial int64, workload string, usage x509.ExtKeyUsage) tls.Certificate {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(workload)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: workload}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, URIs: []*url.URL{uri}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	}
	const workloadID = "spiffe://example.internal/themisy/runner-1"
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := x509.ParseCertificate(serverDER)
	if err != nil {
		t.Fatal(err)
	}
	controlPlane := httptest.NewUnstartedServer((&RunnerServer{ResultVerifier: testReportVerifier(), Store: store.NewMemory(), Auth: MTLSRunnerAuthenticator{workloadID: {RunnerID: "runner-1", TenantID: "tenant-1", RunnerGroup: "prod-jp"}}}).Handler())
	controlPlane.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: serverKey, Leaf: serverCert}}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs}
	controlPlane.StartTLS()
	defer controlPlane.Close()
	trustedServer := controlPlane.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: trustedServer, Certificates: []tls.Certificate{issueLeaf(3, workloadID, x509.ExtKeyUsageClientAuth)}}}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Get(controlPlane.URL + "/v1/runner/grants:next?wait=0s")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("verified Runner identity status=%d, want %d", response.StatusCode, http.StatusNoContent)
	}

	transport.TLSClientConfig.Certificates = []tls.Certificate{issueLeaf(4, "spiffe://example.internal/themisy/unregistered", x509.ExtKeyUsageClientAuth)}
	transport.CloseIdleConnections()
	response, err = (&http.Client{Transport: transport}).Get(controlPlane.URL + "/v1/runner/grants:next?wait=0s")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unregistered workload identity status=%d, want %d", response.StatusCode, http.StatusUnauthorized)
	}

	transport.TLSClientConfig.Certificates = nil
	transport.CloseIdleConnections()
	response, err = (&http.Client{Transport: transport}).Get(controlPlane.URL + "/v1/runner/grants:next?wait=0s")
	if err == nil {
		response.Body.Close()
		t.Fatal("Control Plane accepted a client without a verified certificate")
	}
}

func TestHeartbeatAppliesDrainAndPreventsDispatch(t *testing.T) {
	memory := store.NewMemory()
	authenticator := StaticRunnerAuthenticator{"runner-1": {TenantID: "tenant-1", RunnerGroup: "prod-jp", Token: "secret"}}
	server := httptest.NewServer((&RunnerServer{ResultVerifier: testReportVerifier(), Store: memory, Fleet: memory, Audit: memory, Auth: authenticator, RequireHeartbeat: true, HeartbeatInterval: time.Second}).Handler())
	defer server.Close()
	runtimeState := runnercore.NewRuntimeState(2)
	client := &RunnerClient{ResultSigner: testReportSigner(), TenantID: "tenant-1", RunnerGroup: "prod-jp", BaseURL: server.URL, RunnerID: "runner-1", Token: "secret", Runtime: runtimeState, Capacity: 2}
	if err := client.heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	registered, err := memory.GetRunner(context.Background(), "tenant-1", "runner-1")
	if err != nil || registered.Status != domain.RunnerReady || registered.Capacity != 2 {
		t.Fatalf("registered=%#v err=%v", registered, err)
	}
	now := time.Now().UTC()
	audit := domain.AuditEvent{ID: "drain-audit", CorrelationID: "runner/tenant-1/runner-1", ActorType: "user", ActorID: "operator", Action: "runner.draining", ResourceType: "runner", ResourceID: "runner-1", Result: "DRAINING", Timestamp: now}
	if _, err := memory.SetRunnerStatus(context.Background(), "tenant-1", "runner-1", domain.RunnerDraining, "operator", now, audit); err != nil {
		t.Fatal(err)
	}
	if err := client.heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtimeState.Mode() != protocol.RunnerModeDraining || runtimeState.Accepting() {
		t.Fatalf("mode=%s accepting=%v", runtimeState.Mode(), runtimeState.Accepting())
	}
	actionGrant := protocol.ActionGrant{GrantID: "grant-drain", RunID: "run-drain", StepID: "step-1", TenantID: "tenant-1", RunnerGroup: "prod-jp", IdempotencyKey: "key-drain", Nonce: "nonce-drain", ExpiresAt: now.Add(time.Minute)}
	if _, _, err := memory.CreateGrantDispatch(context.Background(), store.GrantDispatchRecord{Grant: actionGrant, CreatedAt: now, UpdatedAt: now}, domain.AuditEvent{ID: "issue-drain", CorrelationID: actionGrant.RunID, Timestamp: now}, domain.OutboxEvent{ID: "outbox-drain", AggregateType: "action_grant", AggregateID: actionGrant.GrantID, EventType: "grant.dispatch.requested", CreatedAt: now, AvailableAt: now}); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/runner/grants:next?wait=0s", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(runnerIDHeader, "runner-1")
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("dispatch status=%d", response.StatusCode)
	}
	pending, err := memory.GetGrantDispatch(context.Background(), actionGrant.GrantID)
	if err != nil || pending.Status != store.GrantDispatchPending {
		t.Fatalf("drain claimed a new grant: %#v err=%v", pending, err)
	}
}

func TestAuthenticationFailureIsAudited(t *testing.T) {
	memory := store.NewMemory()
	server := httptest.NewServer((&RunnerServer{ResultVerifier: testReportVerifier(), Store: memory, Fleet: memory, Audit: memory, Auth: StaticRunnerAuthenticator{"runner-1": {TenantID: "tenant-1", RunnerGroup: "prod-jp", Token: "secret"}}}).Handler())
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/runner/grants:next?wait=0s", nil)
	request.Header.Set(runnerIDHeader, "runner-1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", response.StatusCode)
	}
	events, err := memory.AuditEvents("runner-auth/runner-1")
	if err != nil || len(events) != 1 || events[0].Result != "REJECTED" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestPreExecutionRefusalRemainsUnknownAndAudited(t *testing.T) {
	memory := store.NewMemory()
	now := time.Now().UTC()
	validSigner, _, _ := grant.NewDevelopmentSigner("fixture")
	issuer, _ := application.NewGrants(memory, validSigner, "https://control.example", time.Minute)
	run := approvedTransportRun(now)
	memory.CreateRun(run)
	issued, _, issueErr := issuer.Issue(context.Background(), application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
	if issueErr != nil {
		t.Fatal(issueErr)
	}
	actionGrant := issued.Grant
	server := httptest.NewServer((&RunnerServer{ResultVerifier: testReportVerifier(), Store: memory, Audit: memory, Auth: StaticRunnerAuthenticator{"runner-1": {TenantID: "tenant-1", RunnerGroup: "prod-jp", Token: "secret"}}}).Handler())
	defer server.Close()
	client := &RunnerClient{ResultSigner: testReportSigner(), TenantID: "tenant-1", RunnerGroup: "prod-jp", BaseURL: server.URL, RunnerID: "runner-1", Token: "secret", Wait: time.Millisecond, BeforeExecute: func(context.Context, protocol.ActionGrant) error { return grant.ErrWrongTenant }}
	delivered, err := client.RunOnce(context.Background(), rejectingProbe{})
	if err != nil || !delivered {
		t.Fatalf("delivered=%v err=%v", delivered, err)
	}
	stored, err := memory.GetGrantDispatch(context.Background(), actionGrant.GrantID)
	if err != nil || stored.Status != store.GrantDispatchUnknown || stored.Result.Status != protocol.ResultUnknown || stored.Result.ReasonCode != "WRONG_TENANT" {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	events, err := memory.AuditEvents(actionGrant.RunID)
	if err != nil || len(events) < 4 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	resultAudit := events[len(events)-1]
	if resultAudit.Action != "grant.result" || resultAudit.Result != "UNKNOWN" || resultAudit.Details["reason_code"] != "WRONG_TENANT" {
		t.Fatalf("result audit=%#v", resultAudit)
	}
}

type rejectingProbe struct{}

func (rejectingProbe) Execute(context.Context, protocol.ActionGrant) (protocol.Result, error) {
	panic("executor must not run after pre-execution rejection")
}

func TestStaticRunnerAuthenticatorRequiresBearerScheme(t *testing.T) {
	authenticator := StaticRunnerAuthenticator{"runner-1": {TenantID: "tenant-1", RunnerGroup: "prod-jp", Token: "secret"}}
	request := httptest.NewRequest(http.MethodGet, "/v1/runner/grants:next", nil)
	request.Header.Set(runnerIDHeader, "runner-1")
	request.Header.Set("Authorization", "secret")
	if _, err := authenticator.Authenticate(request); err == nil {
		t.Fatal("expected non-Bearer authorization to be rejected")
	}
}

func TestSignedGrantReachesSeparateRunnerProcess(t *testing.T) {
	if os.Getenv("THEMISY_RUNNER_HELPER") == "1" {
		runRunnerHelper(t)
		return
	}
	now := time.Now().UTC()
	signer, publicKey, err := grant.NewDevelopmentSigner("transport-key")
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	run := approvedTransportRun(now)
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	issuer, err := application.NewGrants(memory, signer, "https://control.example", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.Now = func() time.Time { return now }
	dispatch, created, err := issuer.Issue(context.Background(), application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
	if err != nil || !created {
		t.Fatalf("issue dispatch=%v err=%v", created, err)
	}
	actionGrant := dispatch.Grant
	manifestSigner, manifestKey, err := grant.NewDevelopmentSigner("manifest-key")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := application.ApprovedManifest(context.Background(), memory, manifestSigner, "https://approvals.example", application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy}, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal([]protocol.PlanManifest{manifest})
	if err != nil {
		t.Fatal(err)
	}
	manifestFile := t.TempDir() + "/manifests.json"
	if err := os.WriteFile(manifestFile, manifestData, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&RunnerServer{ResultVerifier: testReportVerifier(), Store: memory, Auth: StaticRunnerAuthenticator{"runner-1": {TenantID: actionGrant.TenantID, RunnerGroup: actionGrant.RunnerGroup, Token: "test-runner-token"}}, MaxWait: time.Second, PollInterval: 10 * time.Millisecond}).Handler())
	defer server.Close()
	marker := t.TempDir() + "/adapter-called"
	command := exec.Command(os.Args[0], "-test.run=TestSignedGrantReachesSeparateRunnerProcess")
	command.Env = append(os.Environ(),
		"THEMISY_RUNNER_HELPER=1",
		"THEMISY_TEST_CONTROL_PLANE="+server.URL,
		"THEMISY_TEST_GRANT_KEY="+base64.RawURLEncoding.EncodeToString(publicKey),
		"THEMISY_TEST_MARKER="+marker,
		"THEMISY_TEST_MANIFEST_FILE="+manifestFile,
		"THEMISY_TEST_MANIFEST_KEY="+base64.RawURLEncoding.EncodeToString(manifestKey),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("runner subprocess: %v\n%s", err, output)
	}
	record, err := memory.GetGrantDispatch(context.Background(), actionGrant.GrantID)
	if err != nil || record.Status != store.GrantDispatchSucceeded || record.AcknowledgedAt.IsZero() || record.Result.ExternalExecutionID != "separate-process-execution" {
		t.Fatalf("dispatch=%#v err=%v", record, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("Runner.Execute did not reach typed adapter: %v", err)
	}
}

func runRunnerHelper(t *testing.T) {
	t.Helper()
	publicKeyBytes, err := base64.RawURLEncoding.DecodeString(os.Getenv("THEMISY_TEST_GRANT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	digest := testTransportDigest()
	verifier := &grant.Verifier{Issuer: "https://control.example", RunnerGroup: "prod-jp", TenantID: "tenant-1", Keys: grant.StaticKeys{"https://control.example\x00transport-key": ed25519.PublicKey(publicKeyBytes)}, ClockSkew: time.Minute}
	manifestKey, err := base64.RawURLEncoding.DecodeString(os.Getenv("THEMISY_TEST_MANIFEST_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	contexts := &runnercore.ManifestContexts{Store: store.NewMemory(), Issuer: "https://approvals.example", TenantID: "tenant-1", RunnerGroup: "prod-jp", Keys: grant.StaticKeys{"https://approvals.example\x00manifest-key": ed25519.PublicKey(manifestKey)}}
	if err := contexts.LoadFile(context.Background(), os.Getenv("THEMISY_TEST_MANIFEST_FILE")); err != nil {
		t.Fatal(err)
	}
	connected := &runnercore.AtomicConnectionState{}
	broker, err := credentials.NewStaticDevelopmentBroker(map[string][]byte{"token": []byte("local-only")}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	executionRunner := &runnercore.Runner{
		TenantID: "tenant-1", RunnerGroup: "prod-jp", Grants: verifier,
		Subjects: helperSubject{}, Delegations: &identity.DelegationVerifier{Resolver: identity.StaticDelegations{"delegation-1": {ID: "delegation-1", Issuer: "https://idp.example", UserSubject: "user-1", AgentID: "agent-1", Actions: []protocol.Capability{protocol.CapabilityDeploy}, ServiceSelectors: []string{"payment-api"}, Environments: []string{"production"}, MaximumRisk: "high", ExpiresAt: now.Add(time.Hour)}}},
		Contexts: contexts, Approvals: contexts, Capabilities: runnercore.NewCapabilitySet(protocol.CapabilityDeploy),
		Policy: policy.NewEngine(helperPolicy{policyHash: digest}), Journal: store.NewMemory(), Credentials: broker,
		Adapter: helperAdapter{marker: os.Getenv("THEMISY_TEST_MARKER")}, Connectivity: connected,
	}
	client := &RunnerClient{ResultSigner: testReportSigner(), TenantID: "tenant-1", RunnerGroup: "prod-jp", BaseURL: os.Getenv("THEMISY_TEST_CONTROL_PLANE"), RunnerID: "runner-1", Token: "test-runner-token", Wait: time.Second, Connectivity: connected, BeforeExecute: func(ctx context.Context, actionGrant protocol.ActionGrant) error {
		if err := verifier.Verify(ctx, actionGrant); err != nil {
			return err
		}
		return nil
	}}
	delivered, err := client.RunOnce(context.Background(), executionRunner)
	if err != nil || !delivered {
		t.Fatalf("delivered=%v err=%v", delivered, err)
	}
}

type helperSubject struct{}

func (helperSubject) Verify(_ context.Context, _ string) (identity.Subject, error) {
	return identity.Subject{Issuer: "https://idp.example", ID: "user-1", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type helperPolicy struct{ policyHash string }

func (p helperPolicy) Evaluate(_ context.Context, input policy.Input) (policy.Evaluation, error) {
	hash, err := policy.InputHash(input)
	return policy.Evaluation{Decision: domain.DecisionAllow, InputHash: hash, PolicyHash: p.policyHash, PolicyVersion: "test"}, err
}

type helperAdapter struct{ marker string }

func (a helperAdapter) Authorize(protocol.Target, protocol.Action) error { return nil }

func (a helperAdapter) Execute(_ context.Context, _ runnercore.AdapterRequest, _ credentials.Credential) (runnercore.AdapterResult, error) {
	if err := os.WriteFile(a.marker, []byte("called"), 0o600); err != nil {
		return runnercore.AdapterResult{}, err
	}
	return runnercore.AdapterResult{ExternalExecutionID: "separate-process-execution", CompletedAt: time.Now().UTC()}, nil
}

func approvedTransportRun(now time.Time) *domain.ReleaseRun {
	digest := testTransportDigest()
	run := &domain.ReleaseRun{ID: "run-transport", RequestID: "request-transport", WorkflowID: "run-transport", TenantID: "tenant-1", Environment: domain.EnvironmentProduction, RequestedBy: "user-1", SubjectType: "user", SubjectIssuer: "https://idp.example", UserIdentityProof: "signed-oidc", DelegationRef: "delegation-1", Agent: domain.AgentIdentity{ID: "agent-1"}, Status: domain.RunRunning, StateVersion: 1, CreatedAt: now, UpdatedAt: now,
		Plan:  domain.ReleasePlan{Hash: digest, PlanHash: digest, PolicyHash: digest, EvidenceHash: digest, ExpiresAt: now.Add(time.Hour), Phases: []domain.PlanPhase{{Steps: []domain.PlanStep{{Service: "payment-api", ContractHash: digest, ProfileHash: digest, Scheduling: domain.SchedulingContext{RunnerGroup: "prod-jp", RiskTier: "medium"}}}}}},
		Steps: []domain.ReleaseStep{{Service: "payment-api", Status: domain.StepExecuting, Change: domain.Change{Service: "payment-api", DesiredVersion: digest, Risk: "medium", CISuccess: true, DependenciesHealthy: true}}},
	}
	inputHash, _ := policy.InputHash(policy.InputForRelease(*run, run.Steps[0]))
	run.Steps[0].Policy = &domain.PolicyDecision{Decision: domain.DecisionAllow, InputHash: inputHash}
	return run
}

func testTransportDigest() string { return "sha256:" + strings.Repeat("a", 64) }
