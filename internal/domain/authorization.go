package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// AuthorizationStop records a refusal before a new operation. It never changes
// the immutable Plan or authorizes a successor/retry of an unresolved write.
type AuthorizationStop struct {
	ReasonCode string    `json:"reason_code"`
	Object     string    `json:"object"`
	Service    string    `json:"service,omitempty"`
	Deadline   time.Time `json:"deadline"`
	ObservedAt time.Time `json:"observed_at"`
}

// ForwardDeadline separates the approval wait bound from usable write authority.
// Zero Plan deadlines are supported only by legacy development fixtures.
func ForwardDeadline(plan ReleasePlan, approval *Approval) (time.Time, string) {
	until, object := plan.ExpiresAt, "plan"
	if plan.Context != nil && (until.IsZero() || plan.Context.ExpiresAt.Before(until)) {
		until, object = plan.Context.ExpiresAt, "context"
	}
	if approval != nil && (until.IsZero() || approval.ExpiresAt.Before(until)) {
		until, object = approval.ExpiresAt, "approval"
	}
	return until, object
}

func ExpiredAuthority(plan ReleasePlan, approval *Approval, service string, now time.Time) *AuthorizationStop {
	until, object := ForwardDeadline(plan, approval)
	if until.IsZero() || now.Before(until) {
		return nil
	}
	return &AuthorizationStop{ReasonCode: "REAUTHORIZATION_REQUIRED", Object: object, Service: service, Deadline: until, ObservedAt: now.UTC()}
}

// AuthorizationEpoch binds the original time interval as well as content. A
// matching Plan content hash cannot silently extend an approval to a new epoch.
func AuthorizationEpoch(run ReleaseRun, step ReleaseStep) string {
	if run.Plan.CreatedAt.IsZero() || run.Plan.ExpiresAt.IsZero() {
		return ""
	}
	data, _ := json.Marshal(struct {
		RunID     string                `json:"run_id"`
		PlanHash  string                `json:"plan_hash"`
		CreatedAt time.Time             `json:"created_at"`
		ExpiresAt time.Time             `json:"expires_at"`
		Context   *SourceContextBinding `json:"context,omitempty"`
		Approval  *Approval             `json:"approval,omitempty"`
	}{run.ID, run.Plan.Hash, run.Plan.CreatedAt.UTC(), run.Plan.ExpiresAt.UTC(), run.Plan.Context, step.Approval})
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}
