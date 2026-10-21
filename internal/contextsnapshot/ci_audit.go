package contextsnapshot

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"themisy/adapters/githubactions"
	"themisy/internal/domain"
)

var ErrCIAuditUnavailable = errors.New("CI_OBSERVATION_AUDIT_UNAVAILABLE")

// CIAuditSink must commit before returning nil and respect cancellation.
// The existing context-aware PostgreSQL audit store satisfies this contract.
// Stored records are observations, never a trusted snapshot restore source.
type CIAuditSink interface {
	AppendAuditContext(context.Context, domain.AuditEvent) error
}

// AuditedCICollector returns newly collected observations only after their
// inspection record has been committed. It confers no execution authority.
type AuditedCICollector struct {
	collector *CICollector
	audit     CIAuditSink
}

func NewAuditedCICollector(collector *CICollector, sink CIAuditSink) (*AuditedCICollector, error) {
	if collector == nil || collector.client == nil || collector.broker == nil || sink == nil {
		return nil, ErrCIAuditUnavailable
	}
	return &AuditedCICollector{collector: collector, audit: sink}, nil
}

func (c *AuditedCICollector) Collect(ctx context.Context, request githubactions.CIRequest) (CISnapshot, error) {
	if c == nil || c.collector == nil || c.audit == nil {
		return CISnapshot{}, ErrCIAuditUnavailable
	}
	snapshot, err := c.collector.Collect(ctx, request)
	if err != nil {
		return CISnapshot{}, err
	}
	// Record returns a copy, including artifact slices. A sink cannot change the
	// private snapshot, and the caller cannot mutate the stored observation.
	record := snapshot.Record()
	id := "ci_observation_" + rand.Text()
	event := domain.AuditEvent{
		ID: id, CorrelationID: "context/" + record.TenantID + "/" + id,
		ActorType: "system", ActorID: "ci-collector", Action: "context.ci.observe",
		ResourceType: "ci_snapshot", ResourceID: record.Hash,
		Result: string(record.Evidence.Status), Timestamp: record.CapturedAt,
		Details: map[string]any{"record": record},
	}
	appendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// An ambiguous append is not retried or read back to justify returning data.
	if err := c.audit.AppendAuditContext(appendCtx, event); err != nil || appendCtx.Err() != nil || ctx.Err() != nil {
		return CISnapshot{}, ErrCIAuditUnavailable
	}
	return snapshot, nil
}
