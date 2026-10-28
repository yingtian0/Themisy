package store

import (
	"context"
	"sync"
)

// MemoryDirectoryLedger is a test/development ledger, never production durability.
type MemoryDirectoryLedger struct {
	mu       sync.Mutex
	versions map[string]directoryVersion
}
type directoryVersion struct {
	revision int64
	hash     string
}

func (l *MemoryDirectoryLedger) AcceptIdentityDirectory(ctx context.Context, tenant, issuer string, revision int64, hash string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ctx.Err() != nil || tenant == "" || issuer == "" || revision < 1 || hash == "" {
		return ErrConflict
	}
	if l.versions == nil {
		l.versions = map[string]directoryVersion{}
	}
	key := tenant + "\x00" + issuer
	old, ok := l.versions[key]
	if ok && (revision < old.revision || (revision == old.revision && hash != old.hash)) {
		return ErrConflict
	}
	l.versions[key] = directoryVersion{revision, hash}
	return nil
}
