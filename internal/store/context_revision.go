package store

import (
	"context"
	"sync"
)

// MemoryContextLedger is a fixture-only fence. Production uses separate durable
// Control Plane and customer Runner schemas.
type MemoryContextLedger struct {
	mu        sync.Mutex
	revisions map[string]directoryVersion
}

func (l *MemoryContextLedger) AcceptContextRevision(ctx context.Context, tenant, issuer string, revision int64, hash string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ctx.Err() != nil || tenant == "" || issuer == "" || revision < 1 || hash == "" {
		return ErrConflict
	}
	if l.revisions == nil {
		l.revisions = map[string]directoryVersion{}
	}
	key := tenant + "\x00" + issuer
	old := l.revisions[key]
	if revision < old.revision || (revision == old.revision && hash != old.hash) {
		return ErrConflict
	}
	l.revisions[key] = directoryVersion{revision: revision, hash: hash}
	return nil
}

func (m *Memory) AcceptContextRevision(ctx context.Context, tenant, issuer string, revision int64, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil || m.journalError != nil || tenant == "" || issuer == "" || revision < 1 || hash == "" {
		return ErrConflict
	}
	if m.contextRevisions == nil {
		m.contextRevisions = map[string]directoryVersion{}
	}
	key := tenant + "\x00" + issuer
	old := m.contextRevisions[key]
	if revision < old.revision || (revision == old.revision && hash != old.hash) {
		return ErrConflict
	}
	m.contextRevisions[key] = directoryVersion{revision: revision, hash: hash}
	return nil
}
