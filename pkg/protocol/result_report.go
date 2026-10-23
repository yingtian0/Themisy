package protocol

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"themisy/pkg/adapter"
)

const (
	ResultReportVersionV1 = "themisy.result-report/v1"
	ResultReportVersion   = "themisy.result-report/v2"
)

// ResultReport identifies the reporting Runner, which may recover another
// Runner's journal entry. It does not attest that the reporter executed the write.
type ResultReport struct {
	Version     string    `json:"version"`
	RunnerID    string    `json:"runner_id"`
	TenantID    string    `json:"tenant_id"`
	RunnerGroup string    `json:"runner_group"`
	RequestHash string    `json:"request_hash"`
	Result      Result    `json:"result"`
	Signature   Signature `json:"signature"`
}

// CanonicalResultReportPayload is a domain-separated fixed-field JSON encoding.
// Key ID and algorithm are signed; only the signature value is excluded.
// Times use UTC RFC3339Nano. No map ordering or transport whitespace is signed.
func CanonicalResultReportPayload(report ResultReport) ([]byte, error) {
	if (report.Version != ResultReportVersionV1 && report.Version != ResultReportVersion) || report.RunnerID == "" || report.TenantID == "" || report.RunnerGroup == "" || report.Signature.KeyID == "" || report.Signature.Algorithm != "Ed25519" {
		return nil, fmt.Errorf("invalid result report identity, version or signing metadata")
	}
	r := report.Result
	if r.ProtocolVersion != VersionV1Alpha1 || r.GrantID == "" || r.RunID == "" || r.StepID == "" || r.CompletedAt.IsZero() || r.Signature != (Signature{}) {
		return nil, fmt.Errorf("invalid result report body")
	}
	switch r.Status {
	case ResultSucceeded, ResultRejected, ResultUnknown, ResultReconciliation:
	default:
		return nil, fmt.Errorf("invalid result status")
	}
	if r.Status == ResultSucceeded && r.ExternalExecutionID == "" {
		return nil, fmt.Errorf("successful report requires external execution ID")
	}
	if report.Version == ResultReportVersionV1 && r.Evidence != nil {
		return nil, fmt.Errorf("result-report/v1 does not support an Evidence body")
	}
	if r.Evidence != nil {
		evidence := *r.Evidence
		if err := adapter.ValidateEvidence(evidence); err != nil {
			return nil, fmt.Errorf("invalid result evidence: %w", err)
		}
		if evidence.Window.To.After(evidence.ObservedAt) || evidence.ObservedAt.After(r.CompletedAt) {
			return nil, fmt.Errorf("invalid result evidence chronology")
		}
		hash, err := adapter.EvidenceHash(evidence)
		if err != nil || hash != r.EvidenceHash || hash != evidence.EvidenceHash {
			return nil, fmt.Errorf("result evidence hash mismatch")
		}
		evidence.Window.From = evidence.Window.From.UTC()
		evidence.Window.To = evidence.Window.To.UTC()
		evidence.ObservedAt = evidence.ObservedAt.UTC()
		r.Evidence = &evidence
	}
	for _, value := range []string{report.RunnerID, report.TenantID, report.RunnerGroup, report.Signature.KeyID, r.GrantID, r.RunID, r.StepID, r.ReasonCode, r.ExternalExecutionID} {
		if len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("invalid report metadata")
		}
	}
	for i, digest := range []string{report.RequestHash, r.EvidenceHash} {
		if i == 1 && digest == "" {
			continue
		}
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
			return nil, fmt.Errorf("invalid result digest")
		}
		if _, err := hex.DecodeString(digest[7:]); err != nil {
			return nil, fmt.Errorf("invalid result digest")
		}
	}
	report.Result = r
	report.Result.CompletedAt = r.CompletedAt.UTC()
	report.Signature.Value = ""
	payload, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	if len(payload)+len(report.Version)+1 > 4096 {
		return nil, fmt.Errorf("result report exceeds signing size limit")
	}
	return append([]byte(report.Version+"\x00"), payload...), nil
}
