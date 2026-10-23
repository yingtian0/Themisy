package policy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"themisy/internal/domain"
)

var ErrShadowAudit = errors.New("POLICY_SHADOW_AUDIT_UNAVAILABLE")

// ShadowAuditSink commits before returning nil and respects context cancellation.
// Audit records are observations, never a policy activation or approval store.
type ShadowAuditSink interface {
	AppendAuditContext(context.Context, domain.AuditEvent) error
}
type ShadowAuditRecord struct {
	Report ShadowReport `json:"report"`
	Hash   string       `json:"hash"`
}
type AuditedShadowComparator struct {
	issuer string
	keys   BundleKeyResolver
	sink   ShadowAuditSink
	now    func() time.Time
}

func NewAuditedShadowComparator(issuer string, keys BundleKeyResolver, sink ShadowAuditSink) (*AuditedShadowComparator, error) {
	if issuer == "" || keys == nil || sink == nil {
		return nil, ErrShadowAudit
	}
	return &AuditedShadowComparator{issuer: issuer, keys: keys, sink: sink, now: time.Now}, nil
}
func shadowReportHash(report ShadowReport) (string, error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Compare revalidates both signed Bundles and appends an independent report copy
// before returning. The clock is host-owned: callers cannot backdate this path.
// It neither installs a Bundle nor authorizes promotion, rollback or a Grant.
func (c *AuditedShadowComparator) Compare(ctx context.Context, current, candidate Bundle, cases []ShadowCase) (ShadowAuditRecord, error) {
	if c == nil || c.keys == nil || c.sink == nil || ctx.Err() != nil {
		return ShadowAuditRecord{}, ErrShadowAudit
	}
	started := c.now().UTC()
	if started.IsZero() {
		return ShadowAuditRecord{}, ErrShadowAudit
	}
	report, err := CompareSignedBundles(ctx, current, candidate, c.issuer, c.keys, cases, started)
	if err != nil {
		return ShadowAuditRecord{}, ErrShadowAudit
	}
	fresh := func(at time.Time) bool {
		return !at.Before(started) && current.ExpiresAt.After(at) && candidate.ExpiresAt.After(at)
	}
	captured := c.now().UTC()
	if !fresh(captured) || ctx.Err() != nil {
		return ShadowAuditRecord{}, ErrShadowAudit
	}
	hash, err := shadowReportHash(report)
	if err != nil {
		return ShadowAuditRecord{}, ErrShadowAudit
	}
	record := ShadowAuditRecord{Report: report, Hash: hash}
	// JSON copy owns every nested Case/Evaluation/reason/role slice independently
	// of the returned report. Neither caller nor sink can alter the other's data.
	raw, err := json.Marshal(record)
	if err != nil {
		return ShadowAuditRecord{}, ErrShadowAudit
	}
	var stored ShadowAuditRecord
	if json.Unmarshal(raw, &stored) != nil {
		return ShadowAuditRecord{}, ErrShadowAudit
	}
	id := "policy_shadow_" + rand.Text()
	event := domain.AuditEvent{ID: id, CorrelationID: "policy-shadow/" + id, ActorType: "system", ActorID: "policy-shadow-comparator", Action: "policy.shadow.observe", ResourceType: "policy_shadow_report", ResourceID: hash, Result: "OBSERVED", Timestamp: captured, Details: map[string]any{"record": stored}}
	appendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if c.sink.AppendAuditContext(appendCtx, event) != nil || appendCtx.Err() != nil || ctx.Err() != nil || !fresh(c.now().UTC()) {
		return ShadowAuditRecord{}, ErrShadowAudit
	}
	return record, nil
}
