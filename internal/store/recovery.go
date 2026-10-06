package store

import (
	"context"
	"themisy/internal/domain"
	"themisy/pkg/protocol"
)

func (m *Memory) ReconcileGrantDispatch(_ context.Context, id string, expected int64, result protocol.Result, audit domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.grantDispatch[id]
	if !ok {
		return ErrNotFound
	}
	if r.Grant.RunID != result.RunID || r.Grant.StepID != result.StepID || result.GrantID != id {
		return ErrConflict
	}
	if r.Result == result {
		return nil
	}
	if r.StateVersion != expected || r.Status == GrantDispatchSucceeded || r.Status == GrantDispatchRejected {
		return ErrConflict
	}
	r.Status = grantStatusForResult(result.Status)
	r.Result = result
	r.StateVersion++
	r.UpdatedAt = audit.Timestamp
	r.CompletedAt = audit.Timestamp
	m.grantDispatch[id] = r
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], audit)
	return nil
}
