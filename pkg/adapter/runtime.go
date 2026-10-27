package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type RuntimeSummary struct {
	DesiredCount int32  `json:"desired_count"`
	RunningCount int32  `json:"running_count"`
	PendingCount int32  `json:"pending_count"`
	TaskCount    int    `json:"task_count"`
	RolloutState string `json:"rollout_state"`
}

// RuntimeObservation is inspectable source data, not an execution authorization.
// Failed observations carry no invented partial runtime counts.
type RuntimeObservation struct {
	ContainerDigests map[string]string  `json:"container_digests,omitempty"`
	SchemaVersion    string             `json:"schema_version"`
	Target           Target             `json:"target"`
	Operation        ReconcileOperation `json:"operation"`
	RequestHash      string             `json:"request_hash"`
	DefinitionHash   string             `json:"definition_hash"`
	ContainerSetHash string             `json:"container_set_hash"`
	Status           VerificationStatus `json:"status"`
	ReasonCode       string             `json:"reason_code"`
	Summary          RuntimeSummary     `json:"summary"`
	ObservedAt       time.Time          `json:"observed_at"`
	AdapterVersion   string             `json:"adapter_version"`
	Hash             string             `json:"hash"`
}

func RuntimeObservationHash(v RuntimeObservation) (string, error) {
	v.Hash = ""
	v.ObservedAt = v.ObservedAt.UTC()
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// RuntimeRequestHash binds observations to a normalized source operation.
func RuntimeRequestHash(r ReconcileRequest) (string, error) {
	raw, err := json.Marshal(struct {
		Target                                              Target
		Operation                                           ReconcileOperation
		ArtifactDigest, IdempotencyKey, OriginalExecutionID string
	}{r.Target, r.Operation, r.ArtifactDigest, r.IdempotencyKey, r.OriginalDeployment.ExternalExecutionID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CombinedEvidence retains the health observation and cryptographically binds
// its complete body and runtime body into a four-valued decision.
func CombinedEvidence(runtime RuntimeObservation, health Evidence) (Evidence, error) {
	if err := ValidateEvidence(health); err != nil {
		return Evidence{}, err
	}
	actual, err := EvidenceHash(health)
	if err != nil || actual != health.EvidenceHash {
		return Evidence{}, fmt.Errorf("health evidence hash mismatch")
	}
	rh, err := RuntimeObservationHash(runtime)
	if err != nil || rh != runtime.Hash || !runtime.Status.Valid() {
		return Evidence{}, fmt.Errorf("runtime evidence hash mismatch")
	}
	e := health
	e.Source = "ecs+datadog"
	e.AdapterVersion = "themisy.observation/v1"
	rank := map[VerificationStatus]int{VerificationPass: 0, VerificationInconclusive: 1, VerificationMissing: 2, VerificationFail: 3}
	if rank[runtime.Status] > rank[health.Status] {
		e.Status = runtime.Status
		e.ReasonCode = runtime.ReasonCode
	}
	if runtime.ObservedAt.After(e.ObservedAt) {
		e.ObservedAt = runtime.ObservedAt
	}
	sum := sha256.Sum256([]byte(runtime.Hash + "\x00" + health.EvidenceHash))
	e.QueryHash = "sha256:" + hex.EncodeToString(sum[:])
	e.EvidenceHash, err = EvidenceHash(e)
	return e, err
}

func (v RuntimeObservation) Clone() RuntimeObservation {
	if v.ContainerDigests != nil {
		owned := make(map[string]string, len(v.ContainerDigests))
		for name, digest := range v.ContainerDigests {
			owned[name] = digest
		}
		v.ContainerDigests = owned
	}
	return v
}
func ContainerSetHash(containers map[string]string) (string, error) {
	raw, err := json.Marshal(containers)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
