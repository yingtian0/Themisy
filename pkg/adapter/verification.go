package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"themisy/pkg/credentials"
)

type VerificationStatus string

const (
	VerificationPass         VerificationStatus = "PASS"
	VerificationFail         VerificationStatus = "FAIL"
	VerificationInconclusive VerificationStatus = "INCONCLUSIVE"
	VerificationMissing      VerificationStatus = "MISSING"
)

func (s VerificationStatus) Valid() bool {
	switch s {
	case VerificationPass, VerificationFail, VerificationInconclusive, VerificationMissing:
		return true
	default:
		return false
	}
}

type ObservationWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type VerificationRequest struct {
	Target     Target            `json:"target"`
	Deployment Deployment        `json:"deployment"`
	Window     ObservationWindow `json:"window"`
}

type Evidence struct {
	Status         VerificationStatus `json:"status"`
	ReasonCode     string             `json:"reason_code"`
	Source         string             `json:"source"`
	QueryHash      string             `json:"query_hash"`
	Window         ObservationWindow  `json:"window"`
	ObservedAt     time.Time          `json:"observed_at"`
	ObservedValue  float64            `json:"observed_value,omitempty"`
	Threshold      float64            `json:"threshold,omitempty"`
	AdapterVersion string             `json:"adapter_version"`
	EvidenceHash   string             `json:"evidence_hash"`
}

type VerificationAdapter interface {
	Name() string
	Version() string
	Verify(context.Context, VerificationRequest, credentials.Credential) (Evidence, error)
}

func ValidateEvidence(evidence Evidence) error {
	if !evidence.Status.Valid() || strings.TrimSpace(evidence.ReasonCode) == "" || strings.TrimSpace(evidence.Source) == "" || strings.TrimSpace(evidence.QueryHash) == "" || evidence.Window.From.IsZero() || evidence.Window.To.IsZero() || !evidence.Window.To.After(evidence.Window.From) || evidence.ObservedAt.IsZero() || strings.TrimSpace(evidence.AdapterVersion) == "" || strings.TrimSpace(evidence.EvidenceHash) == "" {
		return errors.New("evidence is incomplete")
	}
	if math.IsNaN(evidence.ObservedValue) || math.IsInf(evidence.ObservedValue, 0) || math.IsNaN(evidence.Threshold) || math.IsInf(evidence.Threshold, 0) {
		return errors.New("evidence contains non-finite values")
	}
	return nil
}

func EvidenceHash(evidence Evidence) (string, error) {
	evidence.EvidenceHash = ""
	evidence.Window.From = evidence.Window.From.UTC()
	evidence.Window.To = evidence.Window.To.UTC()
	evidence.ObservedAt = evidence.ObservedAt.UTC()
	payload, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
