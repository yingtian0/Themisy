package resultreport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"themisy/internal/grant"
	"themisy/pkg/protocol"
	"time"
)

type countingSigner struct {
	grant.Signer
	calls int
}

func (s *countingSigner) Sign(ctx context.Context, p []byte) (protocol.Signature, error) {
	s.calls++
	return s.Signer.Sign(ctx, p)
}
func fixture(t *testing.T) (Identity, protocol.ActionGrant, protocol.Result) {
	t.Helper()
	now := time.Now().UTC()
	d := "sha256:" + strings.Repeat("a", 64)
	g := protocol.ActionGrant{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: "grant", Issuer: "issuer", TenantID: "tenant", RunnerGroup: "group", RunID: "run", StepID: "step", SubjectType: "user", UserSubject: "user", UserIdentityProof: "proof", AgentID: "agent", DelegationRef: "delegation", Target: protocol.Target{Service: "service", Environment: "production"}, Action: protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: d}, Risk: "low", PlanHash: d, ContractHash: d, ProfileHash: d, PolicyHash: d, PolicyInputHash: d, EvidenceHash: d, IdempotencyKey: "idempotency", Nonce: "nonce", IssuedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	return Identity{"peer", "tenant", "group"}, g, protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, Status: protocol.ResultSucceeded, ExternalExecutionID: "provider-id", CompletedAt: now.Add(-time.Hour), EvidenceHash: d}
}
func TestReportSingleSignatureBoundFieldsAndReloadedRevocation(t *testing.T) {
	ctx := context.Background()
	identity, g, r := fixture(t)
	now := time.Now().UTC()
	hash, err := protocol.GrantHash(g)
	if err != nil {
		t.Fatal(err)
	}
	signer, pub, err := grant.NewDevelopmentSigner("result-1")
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingSigner{Signer: signer}
	s := &Signer{Signer: counter, KeyID: "result-1"}
	report, err := s.Sign(ctx, identity, hash, r)
	if err != nil || counter.calls != 1 {
		t.Fatalf("sign: %v calls=%d", err, counter.calls)
	}
	path := filepath.Join(t.TempDir(), "trust.json")
	bundle := ResultTrustBundle{Version: "themisy.result-trust/v1", RunnerID: identity.RunnerID, TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Keys: []ResultTrustKey{{KeyID: "result-1", PublicKey: base64.RawURLEncoding.EncodeToString(pub), Status: "active", NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}}}
	write := func() {
		raw, _ := json.Marshal(bundle)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	v := &Verifier{Keys: FileKeys{Path: path, Identity: identity, Now: func() time.Time { return now }}}
	if err := v.Verify(ctx, identity, g, report); err != nil {
		t.Fatalf("expired grant recovered by peer: %v", err)
	}
	mutations := map[string]func(*protocol.ResultReport){"runner": func(r *protocol.ResultReport) { r.RunnerID = "other" }, "tenant": func(r *protocol.ResultReport) { r.TenantID = "other" }, "group": func(r *protocol.ResultReport) { r.RunnerGroup = "other" }, "grant": func(r *protocol.ResultReport) { r.Result.GrantID = "other" }, "run": func(r *protocol.ResultReport) { r.Result.RunID = "other" }, "step": func(r *protocol.ResultReport) { r.Result.StepID = "other" }, "status": func(r *protocol.ResultReport) { r.Result.Status = protocol.ResultUnknown }, "provider": func(r *protocol.ResultReport) { r.Result.ExternalExecutionID = "other" }, "time": func(r *protocol.ResultReport) { r.Result.CompletedAt = r.Result.CompletedAt.Add(time.Second) }, "evidence": func(r *protocol.ResultReport) { r.Result.EvidenceHash = "" }, "hash": func(r *protocol.ResultReport) { r.RequestHash = "" }, "key": func(r *protocol.ResultReport) { r.Signature.KeyID = "other" }, "algorithm": func(r *protocol.ResultReport) { r.Signature.Algorithm = "other" }, "version": func(r *protocol.ResultReport) { r.Version = "other" }, "unsigned": func(r *protocol.ResultReport) { r.Signature.Value = "" }}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copy := report
			mutate(&copy)
			if v.Verify(ctx, identity, g, copy) == nil {
				t.Fatal("tampered report accepted")
			}
		})
	}
	bundle.Keys[0].Status = "revoked"
	write()
	if v.Verify(ctx, identity, g, report) == nil {
		t.Fatal("revocation not reloaded")
	}
	if (&Verifier{}).Verify(ctx, identity, g, report) == nil {
		t.Fatal("nil keys accepted")
	}
	var nilVerifier *Verifier
	if nilVerifier.Verify(ctx, identity, g, report) == nil {
		t.Fatal("nil verifier accepted")
	}
	var nilSigner *Signer
	if _, err := nilSigner.Sign(ctx, identity, hash, r); err == nil {
		t.Fatal("nil signer accepted")
	}
}
func TestCanonicalReportUTCAndMalformedBodies(t *testing.T) {
	identity, g, r := fixture(t)
	hash, _ := protocol.GrantHash(g)
	signer, _, _ := grant.NewDevelopmentSigner("key")
	s := &Signer{Signer: signer, KeyID: "key"}
	first, err := s.Sign(context.Background(), identity, hash, r)
	if err != nil {
		t.Fatal(err)
	}
	r.CompletedAt = r.CompletedAt.In(time.FixedZone("offset", 9*3600))
	second, err := s.Sign(context.Background(), identity, hash, r)
	if err != nil || first.Signature != second.Signature || second.Result.CompletedAt.Location() != time.UTC {
		t.Fatalf("UTC canonicalization: %v", err)
	}
	for _, modify := range []func(*protocol.Result){func(r *protocol.Result) { r.ExternalExecutionID = "" }, func(r *protocol.Result) { r.EvidenceHash = "sha256:bad" }, func(r *protocol.Result) { r.Status = "INVALID" }, func(r *protocol.Result) { r.Signature = protocol.Signature{Value: "legacy"} }, func(r *protocol.Result) { r.ReasonCode = "credential\nsecret" }} {
		copy := r
		modify(&copy)
		if _, err := s.Sign(context.Background(), identity, hash, copy); err == nil {
			t.Fatal("malformed result signed")
		}
	}
}

func TestResultTrustRejectsMalformedExpiredAndCrossPurposeBundles(t *testing.T) {
	identity, _, _ := fixture(t)
	now := time.Now().UTC()
	_, pub, _ := grant.NewDevelopmentSigner("key")
	base := ResultTrustBundle{Version: "themisy.result-trust/v1", RunnerID: identity.RunnerID, TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Keys: []ResultTrustKey{{KeyID: "key", PublicKey: base64.RawURLEncoding.EncodeToString(pub), Status: "active", NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}}}
	for _, name := range []string{"grant-purpose", "runner", "tenant", "group", "bundle-expired", "bundle-future", "key-expired", "key-future", "revoked", "unknown-status", "duplicate-id", "duplicate-public", "unknown-field", "duplicate-field", "trailing", "missing-file"} {
		t.Run(name, func(t *testing.T) {
			b := base
			b.Keys = append([]ResultTrustKey(nil), base.Keys...)
			switch name {
			case "grant-purpose":
				b.Version = "themisy.grant-trust/v1"
			case "runner":
				b.RunnerID = "other"
			case "tenant":
				b.TenantID = "other"
			case "group":
				b.RunnerGroup = "other"
			case "bundle-expired":
				b.ExpiresAt = now
			case "bundle-future":
				b.IssuedAt = now.Add(time.Hour)
			case "key-expired":
				b.Keys[0].NotAfter = now
			case "key-future":
				b.Keys[0].NotBefore = now.Add(time.Minute)
			case "revoked":
				b.Keys[0].Status = "revoked"
			case "unknown-status":
				b.Keys[0].Status = "unknown"
			case "duplicate-id":
				b.Keys = append(b.Keys, b.Keys[0])
			case "duplicate-public":
				entry := b.Keys[0]
				entry.KeyID = "alias"
				b.Keys = append(b.Keys, entry)
			}
			raw, _ := json.Marshal(b)
			switch name {
			case "unknown-field":
				raw = append([]byte(`{"unknown":1,`), raw[1:]...)
			case "duplicate-field":
				raw = append([]byte(`{"runner_id":"peer",`), raw[1:]...)
			case "trailing":
				raw = append(raw, []byte(` {}`)...)
			}
			path := filepath.Join(t.TempDir(), "trust.json")
			if name != "missing-file" {
				os.WriteFile(path, raw, 0600)
			}
			keys := FileKeys{Path: path, Identity: identity, Now: func() time.Time { return now }}
			if _, err := keys.ResolveResultKey(context.Background(), identity, "key"); err == nil {
				t.Fatal("invalid trust accepted")
			}
		})
	}
}
