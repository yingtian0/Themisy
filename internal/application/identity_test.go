package application

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/identity"
	runnercore "themisy/internal/runner"
	"themisy/internal/store"
	"themisy/pkg/protocol"
	"time"
)

type hookSigner struct {
	delegate grant.Signer
	hook     func()
}

func (s hookSigner) Sign(ctx context.Context, b []byte) (protocol.Signature, error) {
	if len(b) > 4096 {
		return protocol.Signature{}, fmt.Errorf("signing payload exceeds KMS bound: %d", len(b))
	}
	sig, err := s.delegate.Sign(ctx, b)
	if s.hook != nil {
		s.hook()
	}
	return sig, err
}

func TestProductionGrantRevalidatesDirectoryAndPersistsNoBearer(t *testing.T) {
	for _, scenario := range []string{"success", "requester revoked", "approver revoked", "delegation revoked", "directory unavailable", "expired", "revoked while signing", "plan subject changed", "lower caller risk", "audit unavailable", "idp unavailable", "grant expired during signing"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			run := grantRun(now)
			memory := store.NewMemory()
			signer, pub, _ := grant.NewDevelopmentSigner("key")
			idpPublic, idpKey, _ := ed25519.GenerateKey(rand.Reader)
			oidc := &identity.OIDCVerifier{Issuer: run.SubjectIssuer, Audience: "themisy", Keys: identity.StaticOIDCKeys{run.SubjectIssuer + "\x00idp": idpPublic}, Now: func() time.Time { return now }}
			h, _ := json.Marshal(map[string]any{"alg": "EdDSA", "kid": "idp"})
			payload, _ := json.Marshal(map[string]any{"iss": run.SubjectIssuer, "sub": run.RequestedBy, "aud": "themisy", "exp": now.Add(time.Hour).Unix(), "private": "bearer-canary"})
			input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(payload)
			token := input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(idpKey, []byte(input)))
			dir := &identity.FileDirectory{Path: filepath.Join(t.TempDir(), "directory.json"), Issuer: "directory", TenantID: run.TenantID, MinimumRevision: 1, Keys: grant.StaticKeys{"directory\x00key": pub}, Ledger: &store.MemoryDirectoryLedger{}, Now: func() time.Time { return now }}
			bundle := identity.DirectoryBundle{Version: identity.DirectoryVersion, Issuer: dir.Issuer, TenantID: dir.TenantID, Revision: 1, IssuedAt: now, ExpiresAt: now.Add(20 * time.Minute), Members: []identity.DirectoryMember{{Issuer: run.SubjectIssuer, Subject: run.RequestedBy, Enabled: true, Roles: []string{"deployer"}}, {Issuer: run.SubjectIssuer, Subject: "sre", Enabled: true, Roles: []string{"sre"}}}, Delegations: []identity.Delegation{{TenantID: run.TenantID, ID: run.DelegationRef, Issuer: run.SubjectIssuer, UserSubject: run.RequestedBy, AgentID: run.Agent.ID, Actions: []protocol.Capability{protocol.CapabilityDeploy, protocol.CapabilityRollback}, ServiceSelectors: []string{"payment-api"}, Environments: []string{"production"}, MaximumRisk: "high", ExpiresAt: now.Add(10 * time.Minute)}}}
			write := func() {
				t.Helper()
				b, err := identity.SignDirectory(ctx, signer, "key", bundle)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(b)
				if err := os.WriteFile(dir.Path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write()
			proof, err := identity.Attest(ctx, oidc, signer, "key", identity.Attestation{Issuer: "control", TenantID: run.TenantID, Audience: "themisy", SubjectType: run.SubjectType, RequestID: run.RequestID, AgentID: run.Agent.ID, DelegationRef: run.DelegationRef, Roles: []string{"deployer"}}, token)
			if err != nil {
				t.Fatal(err)
			}
			run.UserIdentityProof = proof
			run.Plan.Requester = &domain.PlanRequester{TenantID: run.TenantID, Subject: run.RequestedBy, Issuer: run.SubjectIssuer, SubjectType: run.SubjectType, AgentID: run.Agent.ID, DelegationRef: run.DelegationRef}
			run.Steps[1].Approval.DecidedBy = "sre"
			run.Steps[1].Approval.DecidedIssuer = run.SubjectIssuer
			run.Steps[1].Approval.RequiredRoles = []string{"sre"}
			service, _ := NewGrants(memory, signer, "control", fiveMinutes)
			service.Signer = hookSigner{delegate: signer}
			service.Now = func() time.Time { return now }
			service.Authority = &IdentityAuthority{Audit: memory, Signer: signer, KeyID: "key", Verifier: &identity.AttestationVerifier{Issuer: "control", TenantID: run.TenantID, Audience: "themisy", Keys: grant.StaticKeys{"control\x00key": pub}, OIDC: oidc, Directory: dir, Now: func() time.Time { return now }}}
			switch scenario {
			case "requester revoked":
				bundle.Revision++
				bundle.Members[0].Enabled = false
				write()
			case "approver revoked":
				bundle.Revision++
				bundle.Members[1].Roles = nil
				write()
			case "delegation revoked":
				bundle.Revision++
				bundle.Delegations = nil
				write()
			case "directory unavailable":
				os.Remove(dir.Path)
			case "expired":
				now = now.Add(5 * time.Minute)
			case "revoked while signing":
				service.Signer = hookSigner{delegate: signer, hook: func() { bundle.Revision++; bundle.Members[0].Enabled = false; write() }}
			case "plan subject changed":
				run.Plan.Requester.Subject = "other"
			case "audit unavailable":
				service.Authority.Audit = unavailableIdentityAudit{}
			case "idp unavailable":
				oidc.Keys = identity.StaticOIDCKeys{}
			case "grant expired during signing":
				service.TTL = time.Second
				service.Signer = hookSigner{delegate: signer, hook: func() { now = now.Add(2 * time.Second) }}
			case "lower caller risk":
				run.Steps[1].Change.Risk = "low"
				bundle.Revision++
				bundle.Delegations[0].MaximumRisk = "low"
				write()
			}
			if _, _, err := memory.CreateRun(run); err != nil {
				t.Fatal(err)
			}
			record, created, err := service.Issue(ctx, GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
			pending, _ := memory.PendingOutboxByType("grant.dispatch.requested", 10)
			if scenario != "success" {
				if err == nil || created || len(pending) != 0 {
					t.Fatalf("unauthorized dispatch: created=%v pending=%d err=%v", created, len(pending), err)
				}
				return
			}
			if err != nil || !created {
				t.Fatalf("created=%v err=%v", created, err)
			}
			source := &observationFlowSource{now: now}
			customerDirectory := *dir
			customerDirectory.Ledger = &store.MemoryDirectoryLedger{}
			customerVerifier := *service.Authority.Verifier
			customerVerifier.Directory = &customerDirectory
			runner := &runnercore.Runner{RunnerID: "runner", TenantID: run.TenantID, RunnerGroup: record.Grant.RunnerGroup, Grants: &grant.Verifier{Issuer: "control", TenantID: run.TenantID, RunnerGroup: record.Grant.RunnerGroup, Keys: grant.StaticKeys{"control\x00key": pub}, Now: func() time.Time { return now }}, Subjects: &customerVerifier, Delegations: &identity.DelegationVerifier{TenantID: run.TenantID, TenantClaim: "tenant_id", Resolver: &customerDirectory, Now: func() time.Time { return now }}, Contexts: observationFlowContext{memory}, Approvals: runnercore.ProofReferences{"approval-1": {}}, Capabilities: runnercore.NewCapabilitySet(protocol.CapabilityDeploy, protocol.CapabilityRollback), Policy: service.Policy, Journal: store.NewMemory(), Credentials: source, Adapter: source, Connectivity: runnercore.ConnectionState(true), Now: func() time.Time { return now }}
			if result, err := runner.Execute(ctx, record.Grant); err != nil || result.Status != protocol.ResultSucceeded || source.writes.Load() != 1 {
				t.Fatalf("production trust chain result=%+v err=%v writes=%d", result, err, source.writes.Load())
			}
			if record.Grant.ExpiresAt.After(now.Add(5 * time.Minute)) {
				t.Fatal("grant outlives subject proof")
			}
			events, _ := memory.AuditEvents(run.ID)
			raw, _ := json.Marshal(struct {
				Record any
				Events any
			}{record, events})
			if strings.Contains(string(raw), token) || strings.Contains(string(raw), "bearer-canary") {
				t.Fatal("raw credential persisted")
			}
		})
	}
}

type unavailableIdentityAudit struct{}

func (unavailableIdentityAudit) AppendAuditContext(context.Context, domain.AuditEvent) error {
	return fmt.Errorf("audit unavailable")
}
