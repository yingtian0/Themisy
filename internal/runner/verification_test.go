package runner

import (
	"context"
	"errors"
	"testing"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
	"time"
)

type observedRuntime struct {
	now    time.Time
	status adapter.VerificationStatus
	calls  int
}

func (r *observedRuntime) ReadRuntime(_ context.Context, q adapter.ReconcileRequest, _ credentials.Credential) (adapter.RuntimeObservation, error) {
	r.calls++
	hash, _ := adapter.RuntimeRequestHash(q)
	v := adapter.RuntimeObservation{SchemaVersion: "themisy.ecs-runtime/v1alpha1", Target: q.Target, Operation: q.Operation, RequestHash: hash, DefinitionHash: testDigest("a"), ContainerSetHash: testDigest("b"), Status: r.status, ReasonCode: "RUNTIME_" + string(r.status), ObservedAt: r.now, AdapterVersion: "test/v1"}
	v.ContainerDigests = map[string]string{"app": testDigest("a")}
	v.ContainerSetHash, _ = adapter.ContainerSetHash(v.ContainerDigests)
	v.Summary = adapter.RuntimeSummary{DesiredCount: 1, RunningCount: 1, TaskCount: 1, RolloutState: "COMPLETED"}
	v.Hash, _ = adapter.RuntimeObservationHash(v)
	return v, nil
}

type observedHealth struct {
	now    time.Time
	status adapter.VerificationStatus
	calls  int
}

func (h *observedHealth) Name() string    { return "health" }
func (h *observedHealth) Version() string { return "v1" }
func (h *observedHealth) Verify(_ context.Context, q adapter.VerificationRequest, _ credentials.Credential) (adapter.Evidence, error) {
	h.calls++
	e := adapter.Evidence{Status: h.status, ReasonCode: "HEALTH_" + string(h.status), Source: "datadog", QueryHash: testDigest("c"), Window: q.Window, ObservedAt: h.now, AdapterVersion: "test/v1"}
	e.EvidenceHash, _ = adapter.EvidenceHash(e)
	return e, nil
}

type readBroker struct {
	calls   int
	request credentials.Request
}

func (b *readBroker) Acquire(_ context.Context, r credentials.Request) (credentials.Credential, error) {
	b.calls++
	b.request = r
	if r.Purpose != credentials.PurposeVerify {
		return credentials.Credential{}, errors.New("write forbidden")
	}
	return credentials.New(map[string][]byte{"read": []byte("fixture")}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)), nil
}

func observationFixture(t *testing.T) (*runnerFixture, protocol.ActionGrant, *observedRuntime, *observedHealth, *readBroker) {
	t.Helper()
	f := newRunnerFixture(t)
	f.runner.RunnerID = "runner-1"
	source, err := f.runner.Execute(context.Background(), f.grant)
	if err != nil {
		t.Fatal(err)
	}
	now := f.now.Add(2 * time.Minute)
	f.runner.Now = func() time.Time { return now }
	f.runner.Grants.(*grant.Verifier).Now = f.runner.Now
	runtime := &observedRuntime{now: now, status: adapter.VerificationPass}
	health := &observedHealth{now: now, status: adapter.VerificationPass}
	broker := &readBroker{}
	f.runner.Observations = &ObservationVerifier{Runtime: runtime, RuntimeBroker: broker, Health: health, HealthBroker: broker}
	f.runner.Capabilities[protocol.CapabilityVerify] = struct{}{}
	hash, _ := protocol.GrantHash(f.grant)
	g := f.grant
	g.GrantID = "verify-1"
	g.Nonce = "verify-nonce"
	g.IdempotencyKey = "verify-key"
	g.IssuedAt = now
	g.ExpiresAt = now.Add(time.Minute)
	g.Action = protocol.Action{Capability: protocol.CapabilityVerify, ArtifactDigest: f.grant.Action.ArtifactDigest, ExternalExecutionID: source.ExternalExecutionID, SourceGrantID: f.grant.GrantID, SourceNonce: f.grant.Nonce, SourceRequestHash: hash, SourceIdempotencyKey: f.grant.IdempotencyKey, SourceCapability: f.grant.Action.Capability, VerificationFrom: source.CompletedAt.Format(time.RFC3339Nano), VerificationTo: f.now.Add(time.Minute).Format(time.RFC3339Nano)}
	g, err = grant.SignActionGrant(context.Background(), f.signer, g)
	if err != nil {
		t.Fatal(err)
	}
	return f, g, runtime, health, broker
}
func TestSourceBoundReadObservationFourValuesAndReplay(t *testing.T) {
	for _, status := range []adapter.VerificationStatus{adapter.VerificationPass, adapter.VerificationFail, adapter.VerificationMissing, adapter.VerificationInconclusive} {
		t.Run(string(status), func(t *testing.T) {
			f, g, runtime, health, broker := observationFixture(t)
			health.status = status
			writes, creds := f.adapter.Calls(), f.credentials.Calls()
			result, err := f.runner.Execute(context.Background(), g)
			if err != nil {
				t.Fatal(err)
			}
			if result.Evidence == nil || result.Runtime == nil || result.Health == nil || result.Evidence.Status != status || protocol.ValidateObservationResult(g, result) != nil {
				t.Fatalf("invalid observation: %+v", result)
			}
			if _, err := f.runner.Execute(context.Background(), g); err != nil {
				t.Fatal(err)
			}
			if f.adapter.Calls() != writes || f.credentials.Calls() != creds || broker.calls != 2 || runtime.calls != 1 || health.calls != 1 {
				t.Fatal("observation replay repeated a read or performed a write")
			}
			result.Runtime.ContainerDigests["app"] = "mutated"
			result.Runtime.Status = adapter.VerificationFail
			result.Health.Status = adapter.VerificationFail
			again, err := f.runner.Execute(context.Background(), g)
			if err != nil || again.Runtime.Status != adapter.VerificationPass || again.Health.Status != status || again.Runtime.ContainerDigests["app"] == "mutated" {
				t.Fatal("returned observation mutated durable journal")
			}
		})
	}
}
func TestReadObservationRejectsWrongSourceBeforeCredentials(t *testing.T) {
	for _, kind := range []string{"source", "nonce", "hash", "target", "operation", "external", "window", "journal", "disconnected"} {
		t.Run(kind, func(t *testing.T) {
			f, g, _, _, broker := observationFixture(t)
			switch kind {
			case "source":
				g.Action.SourceGrantID = "different"
			case "nonce":
				g.Action.SourceNonce = "different"
			case "hash":
				g.Action.SourceRequestHash = testDigest("b")
			case "target":
				g.Target.Service = "different"
			case "operation":
				g.Action.SourceCapability = protocol.CapabilityRollback
			case "external":
				g.Action.ExternalExecutionID = "different"
			case "window":
				g.Action.VerificationFrom = f.now.Add(-time.Second).Format(time.RFC3339Nano)
			case "journal":
				f.journal.SetRunnerJournalError(errors.New("offline"))
			case "disconnected":
				f.runner.Connectivity = ConnectionState(false)
			}
			g, err := grant.SignActionGrant(context.Background(), f.signer, g)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.runner.Execute(context.Background(), g); err == nil || broker.calls != 0 {
				t.Fatal("unbound observation reached credentials")
			}
		})
	}
}
func TestReadObservationDoesNotRequireExpiredForwardIdentity(t *testing.T) {
	f, g, _, _, _ := observationFixture(t)
	f.runner.Subjects = nil
	f.runner.Delegations = nil
	f.runner.Contexts = nil
	f.runner.Approvals = nil
	if _, err := f.runner.Execute(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	source, err := f.journal.GetRunnerAction(context.Background(), g.TenantID, g.RunnerGroup, g.Action.SourceNonce)
	if err != nil || source.Status != store.RunnerActionSucceeded {
		t.Fatal("source write state changed")
	}
}
