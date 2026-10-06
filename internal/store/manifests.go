package store

import (
	"context"
	"fmt"
	"themisy/pkg/protocol"
)

func (m *Memory) PinManifest(_ context.Context, p protocol.PlanManifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journalError != nil {
		return m.journalError
	}
	hash, err := protocol.ManifestHash(p)
	if err != nil {
		return err
	}
	if m.manifests == nil {
		m.manifests = make(map[string]protocol.PlanManifest)
	}
	prefix := fmt.Sprintf("%s\x00%s\x00%s\x00", p.TenantID, p.RunnerGroup, p.RunID)
	for _, old := range m.manifests {
		if old.TenantID == p.TenantID && old.RunnerGroup == p.RunnerGroup && old.RunID == p.RunID && (old.Revision > p.Revision || (old.Revision == p.Revision && old.PlanHash != p.PlanHash)) {
			return ErrConflict
		}
	}
	key := prefix + p.StepID + "\x00" + string(p.Action.Capability)
	if old, ok := m.manifests[key]; ok && old.Revision == p.Revision {
		h, _ := protocol.ManifestHash(old)
		if h != hash {
			return ErrConflict
		}
	}
	p.ApprovalProofs = append([]string(nil), p.ApprovalProofs...)
	m.manifests[key] = p
	return nil
}
func (m *Memory) GetManifest(_ context.Context, tenant, group, run, step string, capability protocol.Capability) (protocol.PlanManifest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.journalError != nil {
		return protocol.PlanManifest{}, m.journalError
	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", tenant, group, run, step, capability)
	p, ok := m.manifests[key]
	if !ok {
		return p, ErrNotFound
	}
	for _, other := range m.manifests {
		if other.TenantID == tenant && other.RunnerGroup == group && other.RunID == run && other.Revision > p.Revision {
			return protocol.PlanManifest{}, ErrNotFound
		}
	}
	p.ApprovalProofs = append([]string(nil), p.ApprovalProofs...)
	return p, nil
}
