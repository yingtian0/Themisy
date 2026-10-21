package runner

import (
	"context"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

func TestFinalApprovedDeployAndRollbackReachRunner(t *testing.T) {
	f := newRunnerFixture(t)
	engine, err := policy.NewMandatoryEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Policy = engine
	f.delegation.MaximumRisk = "high"
	f.refreshDelegation()
	f.runner.Capabilities = NewCapabilitySet(protocol.CapabilityDeploy, protocol.CapabilityRollback)
	g := f.grant
	run := domain.ReleaseRun{ID: g.RunID, RequestID: "final-policy", TenantID: g.TenantID, Environment: domain.EnvironmentStaging, RequestedBy: g.UserSubject, SubjectType: g.SubjectType, SubjectIssuer: "https://idp.example", UserIdentityProof: g.UserIdentityProof, DelegationRef: g.DelegationRef, Agent: domain.AgentIdentity{ID: g.AgentID}, Status: domain.RunRunning, StateVersion: 1,
		Plan:  domain.ReleasePlan{Hash: g.PlanHash, PlanHash: g.PlanHash, PolicyHash: engine.PolicyHash(), EvidenceHash: g.EvidenceHash, ExpiresAt: f.now.Add(60 * 1e9), Phases: []domain.PlanPhase{{Steps: []domain.PlanStep{{Service: g.Target.Service, ContractHash: g.ContractHash, ProfileHash: g.ProfileHash, Scheduling: domain.SchedulingContext{RunnerGroup: g.RunnerGroup, RiskTier: "high"}}}}}},
		Steps: []domain.ReleaseStep{{Service: g.Target.Service, Status: domain.StepExecuting, Change: domain.Change{Service: g.Target.Service, DesiredVersion: g.Action.ArtifactDigest, Risk: "high", CISuccess: true, DependenciesHealthy: true}}}}
	decision := engine.Evaluate(policy.InputForRelease(run, run.Steps[0]))
	if decision.Decision != domain.DecisionRequireApproval {
		t.Fatalf("preapproval=%+v", decision)
	}
	run.Steps[0].Policy = &decision
	decided := f.now
	run.Steps[0].Approval = &domain.Approval{ID: "approval-1", Status: domain.ApprovalApproved, PlanHash: g.PlanHash, DecidedAt: &decided, ExpiresAt: run.Plan.ExpiresAt}
	control := store.NewMemory()
	if _, _, err := control.CreateRun(&run); err != nil {
		t.Fatal(err)
	}
	issuer, err := application.NewGrants(control, f.signer, g.Issuer, 30*1e9)
	if err != nil {
		t.Fatal(err)
	}
	issuer.Now = func() time.Time { return f.now }
	var deployHash string
	for _, capability := range []protocol.Capability{protocol.CapabilityDeploy, protocol.CapabilityRollback} {
		request := application.GrantIssueRequest{RunID: run.ID, StepID: g.Target.Service, Capability: capability}
		if capability == protocol.CapabilityRollback {
			current, _ := control.GetRun(run.ID)
			current.Steps[0].Verification = &domain.Verification{Status: domain.VerificationFail, Healthy: false, Reason: "simulated failed runtime observation"}
			current.Steps[0].Status = domain.StepRollingBack
			if err := control.UpdateRun(current, current.StateVersion); err != nil {
				t.Fatal(err)
			}
			request.ExternalExecutionID = "external-1"
		}
		dispatch, _, err := issuer.Issue(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		issued := dispatch.Grant
		hash, _ := policy.InputHash(policy.InputForGrant(issued, "https://idp.example", g.UserSubject, g.AgentID, g.DelegationRef))
		if hash != issued.PolicyInputHash {
			t.Fatal("final Grant/Runner policy input mismatch")
		}
		if capability == protocol.CapabilityDeploy {
			deployHash = hash
		} else if hash == deployHash {
			t.Fatal("rollback reused deploy policy input")
		}
		// Context is provisioned from the independently persisted Run, not the Grant.
		f.runner.Contexts = StaticContexts{run.ID + "\x00" + g.Target.Service: {PlanHash: run.Plan.Hash, ContractHash: g.ContractHash, ProfileHash: g.ProfileHash, PolicyHash: run.Plan.PolicyHash, EvidenceHash: run.Plan.EvidenceHash, ApprovalRequired: true, Target: g.Target, Action: protocol.Action{Capability: capability, ArtifactDigest: run.Steps[0].Change.DesiredVersion, ExternalExecutionID: request.ExternalExecutionID}}}
		for name, mutate := range map[string]func(*protocol.ActionGrant){
			"approval":   func(g *protocol.ActionGrant) { g.ApprovalProofs = []string{"different-approval"} },
			"artifact":   func(g *protocol.ActionGrant) { g.Action.ArtifactDigest = testDigest("b") },
			"context":    func(g *protocol.ActionGrant) { g.EvidenceHash = testDigest("b") },
			"input-hash": func(g *protocol.ActionGrant) { g.PolicyInputHash = testDigest("b") },
			"capability": func(g *protocol.ActionGrant) {
				if g.Action.Capability == protocol.CapabilityDeploy {
					g.Action.Capability = protocol.CapabilityRollback
					g.Action.ExternalExecutionID = "external-1"
				} else {
					g.Action.Capability = protocol.CapabilityDeploy
					g.Action.ExternalExecutionID = ""
				}
			},
		} {
			t.Run(string(capability)+"/tampered-"+name, func(t *testing.T) {
				bad := issued
				mutate(&bad)
				signed, err := grant.SignActionGrant(context.Background(), f.signer, bad)
				if err != nil {
					t.Fatal(err)
				}
				beforeWrites, beforeCredentials := f.adapter.Calls(), f.credentials.Calls()
				if _, err := f.runner.Execute(context.Background(), signed); err == nil {
					t.Fatal("altered final input accepted")
				}
				if f.adapter.Calls() != beforeWrites || f.credentials.Calls() != beforeCredentials {
					t.Fatal("altered input reached credentials/Write")
				}
			})
		}
		if _, err := f.runner.Execute(context.Background(), issued); err != nil {
			t.Fatal(err)
		}
		// Redelivery returns the recorded result without another credential or Write.
		if _, err := f.runner.Execute(context.Background(), issued); err != nil {
			t.Fatal(err)
		}
	}
	if f.adapter.Calls() != 2 || f.credentials.Calls() != 2 {
		t.Fatalf("writes=%d credentials=%d", f.adapter.Calls(), f.credentials.Calls())
	}
	events, _ := control.AuditEvents(run.ID)
	if len(events) != 2 {
		t.Fatalf("issue audit events=%d", len(events))
	}
	for _, event := range events {
		if event.Details["policy_input_hash"] == nil || event.Details["final_policy_decision"] == nil {
			t.Fatal("final decision not durably audited")
		}
	}
}
