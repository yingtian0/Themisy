package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/policy"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

type operationPolicy struct {
	engine   *policy.Engine
	decision domain.Decision
}

func (p operationPolicy) PolicyHash() string { return p.engine.PolicyHash() }
func (p operationPolicy) Evaluate(ctx context.Context, in policy.Input) (policy.Evaluation, error) {
	result, err := p.engine.EvaluateContext(ctx, in)
	if in.Capability == protocol.CapabilityRollback {
		result.Decision = p.decision
	}
	return result, err
}

type updateWhileSigning struct {
	grant.Signer
	update func()
}

func (s updateWhileSigning) Sign(ctx context.Context, data []byte) (protocol.Signature, error) {
	s.update()
	return s.Signer.Sign(ctx, data)
}

func TestFinalPolicyRefusesStaleAuthorizationAndFacts(t *testing.T) {
	for _, tc := range []string{"revoked", "expired", "pending", "wrong-plan", "changed-policy", "unavailable-policy", "ci-missing", "dependency-missing", "rollback-denied", "rollback-new-approval"} {
		t.Run(tc, func(t *testing.T) {
			now := time.Now().UTC()
			run := grantRun(now)
			memory := store.NewMemory()
			signer, _, _ := grant.NewDevelopmentSigner("key")
			issuer, err := NewGrants(memory, signer, "https://control.example", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			issuer.Now = func() time.Time { return now }
			capability := protocol.CapabilityDeploy
			switch tc {
			case "revoked":
				run.Steps[1].Approval.Status = domain.ApprovalRevoked
			case "expired":
				run.Steps[1].Approval.ExpiresAt = now
			case "pending":
				run.Steps[1].Approval.Status = domain.ApprovalPending
			case "wrong-plan":
				run.Steps[1].Approval.PlanHash = "another"
			case "changed-policy":
				run.Plan.PolicyHash = "sha256:changed"
			case "unavailable-policy":
				issuer.Policy = nil
			case "ci-missing":
				run.Steps[1].Change.CISuccess = false
			case "dependency-missing":
				run.Steps[1].Change.DependenciesHealthy = false
			case "rollback-denied", "rollback-new-approval":
				capability = protocol.CapabilityRollback
				run.Steps[1].Status = domain.StepRollingBack
				decision := domain.DecisionDeny
				if tc == "rollback-new-approval" {
					decision = domain.DecisionRequireApproval
				}
				issuer.Policy = policy.NewEngine(operationPolicy{engine: issuer.Policy, decision: decision})
			}
			if _, _, err := memory.CreateRun(run); err != nil {
				t.Fatal(err)
			}
			if _, _, err := issuer.Issue(context.Background(), GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: capability, ExternalExecutionID: "external"}); !errors.Is(err, ErrGrantNotAuthorized) {
				t.Fatalf("err=%v", err)
			}
			pending, _ := memory.PendingOutboxByType("grant.dispatch.requested", 10)
			if len(pending) != 0 {
				t.Fatal("refusal emitted dispatch")
			}
		})
	}
}

func TestFinalPolicyVersionChangeBeforeCommitCreatesNoGrant(t *testing.T) {
	now := time.Now().UTC()
	run := grantRun(now)
	memory := store.NewMemory()
	memory.CreateRun(run)
	signer, _, _ := grant.NewDevelopmentSigner("key")
	issuer, err := NewGrants(memory, updateWhileSigning{Signer: signer, update: func() {
		current, _ := memory.GetRun(run.ID)
		current.Steps[1].Approval.Status = domain.ApprovalRevoked
		if err := memory.UpdateRun(current, current.StateVersion); err != nil {
			t.Error(err)
		}
	}}, "https://control.example", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.Now = func() time.Time { return now }
	if _, _, err := issuer.Issue(context.Background(), GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
	pending, _ := memory.PendingOutboxByType("grant.dispatch.requested", 10)
	if len(pending) != 0 {
		t.Fatal("changed Run created dispatch")
	}
}

func TestDuplicateGrantCannotReuseChangedApproval(t *testing.T) {
	now := time.Now().UTC()
	run := grantRun(now)
	memory := store.NewMemory()
	memory.CreateRun(run)
	signer, _, _ := grant.NewDevelopmentSigner("key")
	issuer, err := NewGrants(memory, signer, "https://control.example", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.Now = func() time.Time { return now }
	request := GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy}
	first, _, err := issuer.Issue(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	again, fresh, err := issuer.Issue(context.Background(), request)
	if err != nil || fresh || again.Grant.GrantID != first.Grant.GrantID {
		t.Fatalf("duplicate fresh=%v err=%v", fresh, err)
	}
	current, _ := memory.GetRun(run.ID)
	current.Steps[1].Approval.ID = "new-approval"
	if err := memory.UpdateRun(current, current.StateVersion); err != nil {
		t.Fatal(err)
	}
	if _, _, err := issuer.Issue(context.Background(), request); !errors.Is(err, ErrGrantNotAuthorized) {
		t.Fatalf("changed approval err=%v", err)
	}
	pending, _ := memory.PendingOutboxByType("grant.dispatch.requested", 10)
	if len(pending) != 1 {
		t.Fatal("duplicate emitted additional dispatch")
	}
}
