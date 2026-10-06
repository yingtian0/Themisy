package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"themisy/pkg/protocol"
	"time"
)

type ManifestStore interface {
	PinManifest(context.Context, protocol.PlanManifest) error
	GetManifest(context.Context, string, string, string, string, protocol.Capability) (protocol.PlanManifest, error)
}

type ManifestContexts struct {
	Store                         ManifestStore
	Issuer, TenantID, RunnerGroup string
	Keys                          grant.KeyResolver
	Now                           func() time.Time
}

func (m *ManifestContexts) verify(ctx context.Context, p protocol.PlanManifest) error {
	b, err := protocol.CanonicalManifestPayload(p)
	if err != nil {
		return err
	}
	if p.Issuer != m.Issuer || p.TenantID != m.TenantID || p.RunnerGroup != m.RunnerGroup || m.Keys == nil {
		return errors.New("manifest trust boundary mismatch")
	}
	if p.Signature.Algorithm != grant.AlgorithmEd25519 {
		return errors.New("invalid manifest algorithm")
	}
	key, err := m.Keys.ResolveGrantKey(ctx, p.Issuer, p.Signature.KeyID)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(p.Signature.Value)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, b, signature) {
		return errors.New("invalid manifest signature")
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now()
	}
	if p.IssuedAt.After(now) || !p.ExpiresAt.After(now) {
		return errors.New("manifest expired or not yet valid")
	}
	return nil
}

func (m *ManifestContexts) Pin(ctx context.Context, p protocol.PlanManifest) error {
	if err := m.verify(ctx, p); err != nil {
		return err
	}
	if m.Store == nil {
		return errors.New("manifest store unavailable")
	}
	return m.Store.PinManifest(ctx, p)
}

// LoadFile is a separately signed, customer-provisioned distribution path. It
// accepts no Grant as an input and preserves the previous durable revision on
// failure. Callers must fail closed when this refresh fails.
func (m *ManifestContexts) LoadFile(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 4<<20))
	d.DisallowUnknownFields()
	var manifests []protocol.PlanManifest
	if err = d.Decode(&manifests); err != nil {
		return err
	}
	if err = d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing manifest JSON")
	}
	if len(manifests) == 0 {
		return errors.New("empty manifest bundle")
	}
	for _, p := range manifests {
		if err = m.verify(ctx, p); err != nil {
			return err
		}
	}
	for _, p := range manifests {
		if err = m.Pin(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (m *ManifestContexts) manifest(ctx context.Context, g protocol.ActionGrant) (protocol.PlanManifest, error) {
	if m.Store == nil {
		return protocol.PlanManifest{}, errors.New("manifest store unavailable")
	}
	p, err := m.Store.GetManifest(ctx, m.TenantID, m.RunnerGroup, g.RunID, g.StepID, g.Action.Capability)
	if err != nil {
		return p, err
	}
	return p, m.verify(ctx, p)
}

// ResolveForGrant selects an independently pinned operation (deploy/rollback).
func (m *ManifestContexts) ResolveForGrant(ctx context.Context, g protocol.ActionGrant) (PinnedContext, error) {
	p, err := m.manifest(ctx, g)
	if err != nil {
		return PinnedContext{}, err
	}
	return PinnedContext{Revision: p.Revision, PlanHash: p.PlanHash, ContractHash: p.ContractHash, ProfileHash: p.ProfileHash, PolicyHash: p.PolicyHash, EvidenceHash: p.EvidenceHash, Target: p.Target, Action: p.Action, ApprovalRequired: p.ApprovalRequired}, nil
}
func (m *ManifestContexts) ResolveContext(context.Context, string, string) (PinnedContext, error) {
	return PinnedContext{}, errors.New("manifest requires typed grant selector")
}
func (m *ManifestContexts) VerifyApprovals(ctx context.Context, g protocol.ActionGrant, _ identity.Subject) error {
	p, err := m.manifest(ctx, g)
	if err != nil {
		return err
	}
	a := append([]string(nil), g.ApprovalProofs...)
	b := append([]string(nil), p.ApprovalProofs...)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return errors.New("approval binding mismatch")
	}
	return nil
}
