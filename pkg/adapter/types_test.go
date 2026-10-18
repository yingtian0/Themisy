package adapter

import (
	"errors"
	"testing"
	"time"
)

func TestTypedValidationAndErrorClassification(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	request := DeployRequest{Target: Target{Service: "identity", Environment: "staging"}, ArtifactDigest: digest, IdempotencyKey: "key"}
	if err := ValidateDeployRequest(request); err != nil {
		t.Fatal(err)
	}
	request.ArtifactDigest = "latest"
	if err := ValidateDeployRequest(request); err == nil {
		t.Fatal("untyped artifact accepted")
	}
	classified := &Error{Class: ErrorUnknown, Operation: "deploy", Err: errors.New("timeout")}
	if !IsClass(classified, ErrorUnknown) || IsClass(classified, ErrorTerminal) {
		t.Fatal("error classification failed")
	}
}

func TestEvidenceHashCanonicalizesEquivalentInstantsToUTC(t *testing.T) {
	instant := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	evidence := Evidence{Status: VerificationPass, ReasonCode: "THRESHOLD_MET", Source: "datadog", QueryHash: "sha256:query", Window: ObservationWindow{From: instant.Add(-time.Minute), To: instant}, ObservedAt: instant, AdapterVersion: "datadog/v1"}
	first, err := EvidenceHash(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidence.Window.From = evidence.Window.From.In(time.FixedZone("offset", 9*60*60))
	evidence.Window.To = evidence.Window.To.In(time.FixedZone("offset", 9*60*60))
	evidence.ObservedAt = evidence.ObservedAt.In(time.FixedZone("offset", 9*60*60))
	second, err := EvidenceHash(evidence)
	if err != nil || first != second {
		t.Fatalf("equivalent times changed evidence hash: first=%s second=%s err=%v", first, second, err)
	}
}

func TestEvidenceHashDetectsMutation(t *testing.T) {
	now := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	evidence := Evidence{Status: VerificationPass, ReasonCode: "PASS", Source: "datadog", QueryHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Window: ObservationWindow{From: now.Add(-time.Minute), To: now}, ObservedAt: now, ObservedValue: 0.2, Threshold: 1, AdapterVersion: "datadog/v1"}
	evidence.EvidenceHash, _ = EvidenceHash(evidence)
	if err := ValidateEvidence(evidence); err != nil {
		t.Fatal(err)
	}
	original := evidence.EvidenceHash
	evidence.ObservedValue = 2
	mutated, _ := EvidenceHash(evidence)
	if original == mutated {
		t.Fatal("evidence mutation did not change hash")
	}
}
