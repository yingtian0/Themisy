package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"themisy/internal/domain"
	policy "themisy/internal/policymodel"
)

func clonePolicy[T any](value T) T {
	raw, _ := json.Marshal(value)
	var copy T
	json.Unmarshal(raw, &copy)
	return copy
}
func (m *Memory) RegisterPolicyBundle(ctx context.Context, tenant string, b policy.Bundle, audit domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.journalError != nil {
		return ErrConflict
	}
	if m.policyBundles == nil {
		m.policyBundles = map[string]policy.Bundle{}
	}
	key := tenant + "\x00" + b.Hash
	if old, ok := m.policyBundles[key]; ok {
		a, _ := json.Marshal(old)
		v, _ := json.Marshal(b)
		if string(a) != string(v) {
			return policy.ErrAssignmentConflict
		}
		return nil
	}
	for k, old := range m.policyBundles {
		if len(k) >= len(tenant)+1 && k[:len(tenant)+1] == tenant+"\x00" && old.Issuer == b.Issuer && old.Version == b.Version {
			return policy.ErrAssignmentConflict
		}
	}
	m.policyBundles[key] = clonePolicy(b)
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], clonePolicy(audit))
	return nil
}
func (m *Memory) GetPolicyBundle(ctx context.Context, tenant, hash string) (policy.Bundle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.policyBundles[tenant+"\x00"+hash]
	if !ok || ctx.Err() != nil {
		return policy.Bundle{}, ErrNotFound
	}
	return clonePolicy(b), nil
}
func (m *Memory) GetPolicyAssignment(ctx context.Context, tenant, group string) (policy.Assignment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.policyAssignments[tenant+"\x00"+group]
	if !ok || ctx.Err() != nil {
		return policy.Assignment{}, ErrNotFound
	}
	return a, nil
}
func (m *Memory) SetPolicyAssignment(ctx context.Context, a policy.Assignment, expected int64, audit domain.AuditEvent) (policy.Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.journalError != nil {
		return policy.Assignment{}, ErrConflict
	}
	if m.policyAssignments == nil {
		m.policyAssignments = map[string]policy.Assignment{}
	}
	key := a.TenantID + "\x00" + a.RunnerGroup
	old := m.policyAssignments[key]
	if expected != old.Revision || a.Revision != expected+1 || a.BundleHash == old.BundleHash {
		return policy.Assignment{}, policy.ErrAssignmentConflict
	}
	if _, ok := m.policyBundles[a.TenantID+"\x00"+a.BundleHash]; !ok {
		return policy.Assignment{}, ErrNotFound
	}
	a.PreviousHash = old.BundleHash
	m.policyAssignments[key] = a
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], clonePolicy(audit))
	return a, nil
}
func (m *Memory) AcknowledgePolicy(ctx context.Context, tenant, group string, ack policy.Acknowledgement, audit domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.journalError != nil {
		return ErrConflict
	}
	a := m.policyAssignments[tenant+"\x00"+group]
	if a.Revision != ack.Revision || a.BundleHash != ack.BundleHash {
		return policy.ErrAssignmentConflict
	}
	if m.policyAcks == nil {
		m.policyAcks = map[string]policy.Acknowledgement{}
	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", tenant, group, ack.RunnerID, ack.Revision)
	if _, ok := m.policyAcks[key]; ok {
		return nil
	}
	m.policyAcks[key] = ack
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], clonePolicy(audit))
	return nil
}
func (m *Memory) PolicyAcknowledgements(ctx context.Context, tenant, group string, revision int64) ([]policy.Acknowledgement, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	result := []policy.Acknowledgement{}
	prefix := tenant + "\x00" + group + "\x00"
	for key, a := range m.policyAcks {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix && a.Revision == revision {
			result = append(result, a)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RunnerID < result[j].RunnerID })
	return result, nil
}
func (m *Memory) AcceptPolicyRevision(ctx context.Context, a policy.Assignment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.journalError != nil || a.Revision < 1 || a.TenantID == "" || a.RunnerGroup == "" || a.BundleHash == "" {
		return ErrConflict
	}
	if m.policyFences == nil {
		m.policyFences = map[string]policy.Assignment{}
	}
	key := a.TenantID + "\x00" + a.RunnerGroup
	old := m.policyFences[key]
	if a.Revision < old.Revision || (a.Revision == old.Revision && a.BundleHash != old.BundleHash) {
		return policy.ErrAssignmentConflict
	}
	m.policyFences[key] = a
	return nil
}
