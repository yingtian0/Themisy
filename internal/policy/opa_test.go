package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/pkg/protocol"
	policyfiles "themisy/policies"
)

func TestOPAHierarchyIsMonotonicAndUsesCanonicalInput(t *testing.T) {
	baseline, err := policyfiles.FS.ReadFile("baseline/main.rego")
	if err != nil {
		t.Fatal(err)
	}
	lower := `package themisy.authorization
import rego.v1
service_preference := true`
	bundle := Bundle{Version: "bundle-1", Hash: "sha256:test", Modules: []Module{{Name: "main.rego", Layer: LayerPlatform, Source: string(baseline)}, {Name: "service.rego", Layer: LayerService, Source: lower}}}
	opa, err := newOPA(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{UserID: "", AgentID: "agent-1", DelegationRef: "delegation-1", Environment: domain.EnvironmentStaging, Risk: "low"}
	evaluation, err := opa.Evaluate(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Decision != domain.DecisionDeny || !containsString(evaluation.Reasons, "MISSING_SUBJECT") {
		t.Fatalf("evaluation=%#v", evaluation)
	}
	expected, _ := InputHash(input)
	if evaluation.InputHash != expected {
		t.Fatalf("hash=%s want %s", evaluation.InputHash, expected)
	}
}

func TestControlPlaneAndRunnerBuildIdenticalCanonicalInput(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run := domain.ReleaseRun{TenantID: "tenant-1", RunnerGroup: "prod", RequestedBy: "user-1", SubjectIssuer: "https://idp.example", DelegationRef: "delegation-1", Agent: domain.AgentIdentity{ID: "agent-1", Scopes: []string{"requester-supplied-scope-is-not-canonical"}}, Environment: domain.EnvironmentProduction, Plan: domain.ReleasePlan{Hash: digest, PolicyHash: digest, EvidenceHash: digest, Phases: []domain.PlanPhase{{Steps: []domain.PlanStep{{Service: "identity", ContractHash: digest, ProfileHash: digest}}}}}}
	run.SubjectType = "user"
	step := domain.ReleaseStep{Service: "identity", Change: domain.Change{DesiredVersion: digest, Risk: "medium", CISuccess: true, DependenciesHealthy: true, DestructiveMigration: true}, Approval: &domain.Approval{ID: "approval-1", Status: domain.ApprovalApproved}}
	actionGrant := protocol.ActionGrant{TenantID: run.TenantID, RunnerGroup: run.RunnerGroup, UserSubject: run.RequestedBy, AgentID: run.Agent.ID, DelegationRef: run.DelegationRef, Target: protocol.Target{Service: step.Service, Environment: string(run.Environment)}, Action: protocol.Action{Capability: protocol.CapabilityDeploy, ArtifactDigest: digest}, Risk: step.Change.Risk, PlanHash: digest, ContractHash: digest, ProfileHash: digest, PolicyHash: digest, EvidenceHash: digest, ApprovalProofs: []string{"approval-1"}}
	actionGrant.SubjectType = run.SubjectType
	controlHash, err := InputHash(InputForRelease(run, step))
	if err != nil {
		t.Fatal(err)
	}
	runnerHash, err := InputHash(InputForGrant(actionGrant, run.SubjectIssuer, run.RequestedBy, run.Agent.ID, run.DelegationRef))
	if err != nil {
		t.Fatal(err)
	}
	if controlHash != runnerHash {
		t.Fatalf("control=%s runner=%s", controlHash, runnerHash)
	}
}

func TestOPAUnavailableFailsClosed(t *testing.T) {
	engine := NewEngine((*OPA)(nil))
	decision := engine.Evaluate(Input{UserID: "user", AgentID: "agent"})
	if decision.Decision != domain.DecisionDeny || decision.ReasonCode != "POLICY_UNAVAILABLE" {
		t.Fatalf("decision=%#v", decision)
	}
}

func TestMandatoryOPARejectsProductionWithoutPinnedHashesAndApproval(t *testing.T) {
	engine, err := NewMandatoryEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := engine.EvaluateContext(context.Background(), Input{UserID: "user-1", AgentID: "agent-1", DelegationRef: "delegation-1", Environment: domain.EnvironmentProduction, Risk: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Decision != domain.DecisionDeny || !containsString(evaluation.Reasons, "MISSING_PINNED_CONTEXT") || !containsString(evaluation.Reasons, "MISSING_APPROVAL_PROOF") {
		t.Fatalf("evaluation=%#v", evaluation)
	}
}

func TestMandatoryOPARequestsApprovalBeforeHighRiskWrite(t *testing.T) {
	engine, err := NewMandatoryEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := engine.EvaluateContext(context.Background(), Input{UserID: "user-1", AgentID: "agent-1", Environment: domain.EnvironmentStaging, Risk: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Decision != domain.DecisionRequireApproval || !containsString(evaluation.RequiredRoles, "service-owner") {
		t.Fatalf("evaluation=%#v", evaluation)
	}
}

func TestSignedBundleBindsVersionHashModulesAndExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	baseline, err := policyfiles.FS.ReadFile("baseline/main.rego")
	if err != nil {
		t.Fatal(err)
	}
	signer, public, err := grant.NewDevelopmentSigner("policy-key")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := SignBundle(ctx, signer, Bundle{Version: "bundle-1", Issuer: "customer-policy", Compatibility: []string{InputVersionV1Alpha1}, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Modules: []Module{{Name: "main.rego", Layer: LayerPlatform, Source: string(baseline)}}})
	if err != nil {
		t.Fatal(err)
	}
	keys := StaticBundleKeys{bundle.Issuer + "\x00policy-key": public}
	if err := VerifyBundle(ctx, bundle, bundle.Issuer, keys, now); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVerifiedOPA(ctx, bundle, bundle.Issuer, keys, now); err != nil {
		t.Fatal(err)
	}
	modified := bundle
	modified.Modules = append([]Module(nil), bundle.Modules...)
	modified.Modules[0].Source += "\n# modified"
	if err := VerifyBundle(ctx, modified, bundle.Issuer, keys, now); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("modified bundle: %v", err)
	}
	if err := VerifyBundle(ctx, bundle, bundle.Issuer, keys, bundle.ExpiresAt); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("expired bundle: %v", err)
	}
}

func TestCriticalContractRiskCannotBeLoweredOrSkipApproval(t *testing.T) {
	if EffectiveRisk("low", "critical") != "critical" || EffectiveRisk("critical", "high") != "critical" {
		t.Fatal("critical risk was lowered")
	}
	engine, err := NewMandatoryEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.EvaluateContext(context.Background(), Input{UserID: "user", AgentID: "agent", Environment: domain.EnvironmentStaging, Risk: "critical"})
	if err != nil || result.Decision != domain.DecisionRequireApproval {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
