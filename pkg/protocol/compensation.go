package protocol

import (
	"encoding/hex"
	"fmt"
	"time"
)

// CompensationBinding is independently approved, scoped to one successful
// source dispatch and one Runner-local predecessor mapping. It grants no
// authority beyond the current authorization epoch.
type CompensationBinding struct {
	SourceGrantID     string `json:"source_grant_id"`
	SourceNonce       string `json:"source_nonce"`
	SourceRequestHash string `json:"source_request_hash"`
	TargetHash        string `json:"target_hash"`
	SourceIssuedAt    string `json:"source_issued_at"`
	ValidUntil        string `json:"valid_until"`
}

func ValidateCompensation(c *CompensationBinding, action Action, issued, expires time.Time, required bool) error {
	if c == nil {
		if required && action.Capability == CapabilityRollback {
			return fmt.Errorf("compensation permission required")
		}
		return nil
	}
	if action.Capability != CapabilityRollback || c.SourceGrantID == "" || c.SourceNonce == "" {
		return fmt.Errorf("invalid compensation scope")
	}
	from, e1 := time.Parse(time.RFC3339Nano, c.SourceIssuedAt)
	until, e2 := time.Parse(time.RFC3339Nano, c.ValidUntil)
	if e1 != nil || e2 != nil || from.After(issued) || !until.After(issued) || expires.After(until) {
		return fmt.Errorf("invalid compensation lifetime")
	}
	for _, d := range []string{c.SourceRequestHash, c.TargetHash} {
		if len(d) != 71 || d[:7] != "sha256:" {
			return fmt.Errorf("invalid compensation hash")
		}
		if _, err := hex.DecodeString(d[7:]); err != nil {
			return err
		}
	}
	return nil
}
func SameCompensation(a, b *CompensationBinding) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
