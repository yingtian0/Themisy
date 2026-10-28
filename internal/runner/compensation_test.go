package runner

import (
	"context"
	"testing"
	"themisy/internal/grant"
	"themisy/pkg/protocol"
	"time"
)

func (a *countingAdapter) CompensationTargetHash(protocol.Target) (string, error) {
	return testDigest("b"), nil
}

func TestCompensationRequiresSuccessfulSourceAndReviewedPredecessor(t *testing.T) {
	for _, scenario := range []string{"valid", "unknown-source", "unrelated-source", "unreviewed-predecessor", "deadline", "independent-manifest-mismatch", "revoked-approval"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := newRunnerFixture(t)
			source := f.grant
			hash, _ := protocol.GrantHash(source)
			var external string
			if scenario != "unknown-source" {
				result, err := f.runner.Execute(ctx, source)
				if err != nil {
					t.Fatal(err)
				}
				external = result.ExternalExecutionID
			} else {
				external = "unresolved"
			}
			writes, creds := f.adapter.Calls(), f.credentials.Calls()
			f.grant.ProtocolVersion = protocol.VersionV1Alpha2
			f.grant.AuthorizationEpoch = testDigest("c")
			f.grant.AuthorizationValidUntil = f.now.Add(5 * time.Minute).Format(time.RFC3339Nano)
			f.grant.Compensation = &protocol.CompensationBinding{SourceGrantID: source.GrantID, SourceNonce: source.Nonce, SourceRequestHash: hash, SourceIssuedAt: source.IssuedAt.Format(time.RFC3339Nano), ValidUntil: f.now.Add(2 * time.Minute).Format(time.RFC3339Nano), TargetHash: testDigest("b")}
			f.grant.GrantID = "compensation"
			f.grant.Nonce = "compensation-nonce"
			f.grant.IdempotencyKey += "/rollback"
			f.grant.Action.Capability = protocol.CapabilityRollback
			f.grant.Action.ExternalExecutionID = external
			f.runner.Capabilities = NewCapabilitySet(protocol.CapabilityDeploy, protocol.CapabilityRollback)
			switch scenario {
			case "unrelated-source":
				f.grant.Compensation.SourceRequestHash = testDigest("e")
			case "unreviewed-predecessor":
				f.grant.Compensation.TargetHash = testDigest("e")
			case "revoked-approval":
				f.runner.Approvals = ProofReferences{}
			}
			f.syncPinnedTargetAction()
			contexts := f.runner.Contexts.(StaticContexts)
			key := f.grant.RunID + "\x00" + f.grant.StepID
			pinned := contexts[key]
			pinned.AuthorizationEpoch = f.grant.AuthorizationEpoch
			pinned.AuthorizationValidUntil = f.grant.AuthorizationValidUntil
			copy := *f.grant.Compensation
			pinned.Compensation = &copy
			if scenario == "independent-manifest-mismatch" {
				pinned.Compensation.TargetHash = testDigest("f")
			}
			contexts[key] = pinned
			f.resignWithPolicyHash(t)
			f.runner.Now = func() time.Time { return f.now }
			f.runner.Grants.(*grant.Verifier).Now = func() time.Time { return f.now }
			if scenario == "deadline" {
				f.now = f.now.Add(2 * time.Minute)
			}
			result, err := f.runner.Execute(ctx, f.grant)
			if scenario == "valid" {
				if err != nil || result.Status != protocol.ResultSucceeded || f.adapter.Calls() != writes+1 {
					t.Fatalf("valid compensation failed: %+v %v", result, err)
				}
				// Idempotent redelivery observes the existing result, never another Write.
				if _, err := f.runner.Execute(ctx, f.grant); err != nil {
					t.Fatal(err)
				}
				if f.adapter.Calls() != writes+1 {
					t.Fatal("compensation repeated Write")
				}
				f.grant.GrantID = "another-compensation"
				f.grant.Nonce = "another-nonce"
				f.grant.IdempotencyKey = "another-key"
				f.resignWithPolicyHash(t)
				if _, err := f.runner.Execute(ctx, f.grant); err == nil {
					t.Fatal("second Grant consumed one compensation permission twice")
				}
				if f.adapter.Calls() != writes+1 {
					t.Fatal("new Grant bypassed per-source consumption")
				}

			} else if err == nil || f.adapter.Calls() != writes || f.credentials.Calls() != creds {
				t.Fatalf("unsafe compensation: result=%+v err=%v writes=%d creds=%d", result, err, f.adapter.Calls()-writes, f.credentials.Calls()-creds)
			}
		})
	}
}
