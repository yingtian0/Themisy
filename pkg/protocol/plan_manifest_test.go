package protocol

import (
	"bytes"
	"testing"
)

func manifestFixture() PlanManifest {
	g := protocolFixture()
	return PlanManifest{ProtocolVersion: PlanManifestVersion, Issuer: g.Issuer, TenantID: g.TenantID, RunnerGroup: g.RunnerGroup, RunID: g.RunID, StepID: g.StepID, Revision: 1, PlanHash: g.PlanHash, ContractHash: g.ContractHash, ProfileHash: g.ProfileHash, PolicyHash: g.PolicyHash, EvidenceHash: g.EvidenceHash, Target: g.Target, Action: g.Action, IssuedAt: g.IssuedAt, ExpiresAt: g.ExpiresAt, ApprovalRequired: true, ApprovalProofs: []string{"b", "a"}}
}
func TestManifestCanonicalEncodingAndValidation(t *testing.T) {
	m := manifestFixture()
	payload, err := CanonicalManifestPayload(m)
	if err != nil {
		t.Fatal(err)
	}
	m.ApprovalProofs = []string{"a", "b"}
	m.Signature = Signature{Value: "ignored"}
	reordered, err := CanonicalManifestPayload(m)
	if err != nil || !bytes.Equal(payload, reordered) {
		t.Fatalf("noncanonical payload %v", err)
	}
	tests := map[string]func(*PlanManifest){
		"version":             func(m *PlanManifest) { m.ProtocolVersion = "future" },
		"revision":            func(m *PlanManifest) { m.Revision = 0 },
		"issuer":              func(m *PlanManifest) { m.Issuer = "" },
		"hash":                func(m *PlanManifest) { m.PlanHash = "sha256:invalid" },
		"time":                func(m *PlanManifest) { m.ExpiresAt = m.IssuedAt },
		"duplicate approval":  func(m *PlanManifest) { m.ApprovalProofs = []string{"a", "a"} },
		"empty approval":      func(m *PlanManifest) { m.ApprovalProofs = nil },
		"arbitrary operation": func(m *PlanManifest) { m.Action.Capability = "shell" },
		"rollback binding":    func(m *PlanManifest) { m.Action.Capability = CapabilityRollback; m.Action.ExternalExecutionID = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m := manifestFixture()
			mutate(&m)
			if _, err := CanonicalManifestPayload(m); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
