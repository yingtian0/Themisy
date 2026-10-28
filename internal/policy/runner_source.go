package policy

import (
	"context"
	"crypto/ed25519"
	"themisy/internal/grant"
	"time"
)

type FileBundleKeys struct{ Trust grant.FileKeys }

func (k FileBundleKeys) ResolvePolicyKey(ctx context.Context, issuer, keyID string) (ed25519.PublicKey, error) {
	return k.Trust.ResolveGrantKey(ctx, issuer, keyID)
}

type RunnerPolicySource struct {
	TenantID, RunnerGroup, Issuer string
	Keys                          BundleKeyResolver
	Ledger                        RevisionLedger
	Fetch                         func(context.Context) (Delivery, error)
	Acknowledge                   func(context.Context, Assignment) error
	Now                           func() time.Time
}

func (s *RunnerPolicySource) Load(ctx context.Context, tenant, group string) (Delivery, error) {
	if s == nil || s.Keys == nil || s.Ledger == nil || s.Fetch == nil || s.Acknowledge == nil || tenant != s.TenantID || group != s.RunnerGroup {
		return Delivery{}, ErrDistribution
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	delivery, err := s.Fetch(bounded)
	if err != nil {
		return Delivery{}, ErrDistribution
	}
	a := delivery.Assignment
	if a.TenantID != tenant || a.RunnerGroup != group || a.Revision < 1 || a.BundleHash != delivery.Bundle.Hash {
		return Delivery{}, ErrDistribution
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if _, err := NewVerifiedOPA(bounded, delivery.Bundle, s.Issuer, s.Keys, now); err != nil {
		return Delivery{}, ErrDistribution
	}
	if s.Ledger.AcceptPolicyRevision(bounded, a) != nil || s.Acknowledge(bounded, a) != nil || bounded.Err() != nil {
		return Delivery{}, ErrDistribution
	}
	now = time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if VerifyBundle(bounded, delivery.Bundle, s.Issuer, s.Keys, now) != nil {
		return Delivery{}, ErrDistribution
	}
	return delivery, nil
}
