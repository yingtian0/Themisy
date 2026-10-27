package resultreport

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"testing"
	"themisy/internal/grant"
	"themisy/pkg/adapter"
	"themisy/pkg/protocol"
)

func observationReceipt(t *testing.T) (Identity, protocol.ActionGrant, protocol.Result) {
	t.Helper()
	id, g, r := fixture(t)
	health := *r.Evidence
	g.Action = protocol.Action{Capability: protocol.CapabilityVerify, ArtifactDigest: g.Action.ArtifactDigest, ExternalExecutionID: r.ExternalExecutionID, SourceGrantID: "source", SourceNonce: "source-nonce", SourceRequestHash: g.PlanHash, SourceIdempotencyKey: "source-key", SourceCapability: protocol.CapabilityDeploy, VerificationFrom: health.Window.From.Format("2006-01-02T15:04:05.999999999Z07:00"), VerificationTo: health.Window.To.Format("2006-01-02T15:04:05.999999999Z07:00")}
	q := adapter.ReconcileRequest{Target: adapter.Target{Service: g.Target.Service, Environment: g.Target.Environment}, Operation: adapter.ReconcileDeploy, ArtifactDigest: g.Action.ArtifactDigest, IdempotencyKey: g.Action.SourceIdempotencyKey}
	hash, _ := adapter.RuntimeRequestHash(q)
	runtime := adapter.RuntimeObservation{SchemaVersion: "themisy.ecs-runtime/v1alpha1", Target: q.Target, Operation: q.Operation, RequestHash: hash, DefinitionHash: g.PlanHash, ContainerSetHash: g.PlanHash, Status: adapter.VerificationPass, ReasonCode: "RUNTIME_CONFIRMED", ObservedAt: health.ObservedAt, AdapterVersion: "fixture/v1", Summary: adapter.RuntimeSummary{DesiredCount: 1, RunningCount: 1, TaskCount: 1, RolloutState: "COMPLETED"}}
	runtime.ContainerDigests = map[string]string{"app": g.PlanHash}
	runtime.ContainerSetHash, _ = adapter.ContainerSetHash(runtime.ContainerDigests)
	runtime.Hash, _ = adapter.RuntimeObservationHash(runtime)
	aggregate, err := adapter.CombinedEvidence(runtime, health)
	if err != nil {
		t.Fatal(err)
	}
	r.Evidence = &aggregate
	r.Runtime = &runtime
	r.Health = &health
	r.EvidenceHash = aggregate.EvidenceHash
	return id, g, r
}
func TestObservationSignatureBindsRuntimeHealthAndSource(t *testing.T) {
	id, g, r := observationReceipt(t)
	key, pub, err := grant.NewDevelopmentSigner("observation-key")
	if err != nil {
		t.Fatal(err)
	}
	signer := &Signer{Signer: key, KeyID: "observation-key"}
	hash, _ := protocol.GrantHash(g)
	report, err := signer.Sign(context.Background(), id, hash, r)
	if err != nil {
		t.Fatal(err)
	}
	r.Runtime.ContainerDigests["app"] = "mutated after signing"
	verifier := &Verifier{Keys: ScopedKeys{id: observationKeys{pub}}}
	if err := verifier.Verify(context.Background(), id, g, report); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"counts", "definition", "health-threshold", "health-query", "evidence", "source", "external", "grant-id"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(report)
			var bad protocol.ResultReport
			_ = json.Unmarshal(raw, &bad)
			other := g
			switch kind {
			case "counts":
				bad.Result.Runtime.Summary.RunningCount++
			case "definition":
				bad.Result.Runtime.DefinitionHash = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			case "health-threshold":
				bad.Result.Health.Threshold++
			case "health-query":
				bad.Result.Health.Query = "different query"
			case "evidence":
				bad.Result.Evidence.Status = adapter.VerificationFail
			case "source":
				other.Action.SourceGrantID = "another"
			case "external":
				bad.Result.ExternalExecutionID = "another"
			case "grant-id":
				bad.Result.GrantID = "another"
			}
			if verifier.Verify(context.Background(), id, other, bad) == nil {
				t.Fatal("tampered observation accepted")
			}
		})
	}
}
func TestResignedObservationCannotChangeTargetOrCombinedDecision(t *testing.T) {
	id, g, r := observationReceipt(t)
	key, pub, _ := grant.NewDevelopmentSigner("observation-key")
	signer := &Signer{Signer: key, KeyID: "observation-key"}
	verifier := &Verifier{Keys: ScopedKeys{id: observationKeys{pub}}}
	hash, _ := protocol.GrantHash(g)
	for _, kind := range []string{"target", "decision"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(r)
			var bad protocol.Result
			_ = json.Unmarshal(raw, &bad)
			if kind == "target" {
				bad.Runtime.Target.Service = "another"
				bad.Runtime.Hash, _ = adapter.RuntimeObservationHash(*bad.Runtime)
				combined, _ := adapter.CombinedEvidence(*bad.Runtime, *bad.Health)
				bad.Evidence = &combined
				bad.EvidenceHash = combined.EvidenceHash
			} else {
				bad.Health.Status = adapter.VerificationFail
				bad.Health.EvidenceHash, _ = adapter.EvidenceHash(*bad.Health)
			}
			report, err := signer.Sign(context.Background(), id, hash, bad)
			if err != nil {
				t.Fatal(err)
			}
			if verifier.Verify(context.Background(), id, g, report) == nil {
				t.Fatal("resigned misbound observation accepted")
			}
		})
	}
}

type observationKeys struct{ key ed25519.PublicKey }

func (k observationKeys) ResolveResultKey(_ context.Context, _ Identity, id string) (ed25519.PublicKey, error) {
	if id != "observation-key" {
		return nil, fmt.Errorf("unknown key")
	}
	return k.key, nil
}
