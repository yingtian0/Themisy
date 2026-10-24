package policy

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
)

const MaximumCandidateStatusEntries = 256

var ErrCandidateStatus = errors.New("POLICY_CANDIDATE_STATUS_UNAVAILABLE")

type CandidateStatus struct {
	Hash      string    `json:"hash"`
	State     string    `json:"state"`
	Version   string    `json:"version,omitempty"`
	Issuer    string    `json:"issuer,omitempty"`
	IssuedAt  time.Time `json:"issued_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}
type CandidateInventory struct {
	Mode        string            `json:"mode"`
	InspectedAt time.Time         `json:"inspected_at"`
	Entries     []CandidateStatus `json:"entries"`
}

// Status is bounded read-only inspection, never activation or continuing trust.
// Every candidate is independently revalidated; invalid files expose no metadata.
func (c *CandidateCatalog) Status(ctx context.Context) (CandidateInventory, error) {
	if c == nil || c.root == nil || ctx.Err() != nil {
		return CandidateInventory{}, ErrCandidateStatus
	}
	started := c.now().UTC()
	if started.IsZero() {
		return CandidateInventory{}, ErrCandidateStatus
	}
	directory, err := c.root.Open(".")
	if err != nil {
		return CandidateInventory{}, ErrCandidateStatus
	}
	entries, readErr := directory.ReadDir(MaximumCandidateStatusEntries + 1)
	closeErr := directory.Close()
	if (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil || len(entries) > MaximumCandidateStatusEntries {
		return CandidateInventory{}, ErrCandidateStatus
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") && candidateHash.MatchString("sha256:"+strings.TrimSuffix(entry.Name(), ".json")) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	inventory := CandidateInventory{Mode: "LOCAL_CANDIDATE_STATUS_ONLY", Entries: make([]CandidateStatus, 0, len(names))}
	verified := make(map[string]Bundle)
	for _, name := range names {
		if ctx.Err() != nil {
			return CandidateInventory{}, ErrCandidateStatus
		}
		hash := "sha256:" + strings.TrimSuffix(name, ".json")
		row := CandidateStatus{Hash: hash, State: "UNAVAILABLE"}
		if bundle, err := c.Get(ctx, hash); err == nil {
			verified[hash] = bundle
			row = CandidateStatus{Hash: hash, State: "VERIFIED_CANDIDATE", Version: bundle.Version, Issuer: bundle.Issuer, IssuedAt: bundle.IssuedAt, ExpiresAt: bundle.ExpiresAt}
		}
		inventory.Entries = append(inventory.Entries, row)
	}
	inventory.InspectedAt = c.now().UTC()
	if inventory.InspectedAt.Before(started) || ctx.Err() != nil {
		return CandidateInventory{}, ErrCandidateStatus
	}
	// Compilation/other reads may cross expiry or a trust-key change. Recheck
	// signed artifacts at this final observation time, without recompiling them.
	for i, row := range inventory.Entries {
		if bundle, ok := verified[row.Hash]; ok {
			if VerifyBundle(ctx, bundle, c.issuer, c.keys, inventory.InspectedAt) != nil {
				inventory.Entries[i] = CandidateStatus{Hash: row.Hash, State: "UNAVAILABLE"}
			}
		}
	}
	if ctx.Err() != nil {
		return CandidateInventory{}, ErrCandidateStatus
	}
	return inventory, nil
}
