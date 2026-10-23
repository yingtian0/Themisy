// Package resultreport authenticates Runner reports using dedicated Runner trust.
package resultreport

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"themisy/internal/grant"
	"themisy/pkg/protocol"
)

type Identity struct{ RunnerID, TenantID, RunnerGroup string }
type KeyResolver interface {
	ResolveResultKey(context.Context, Identity, string) (ed25519.PublicKey, error)
}
type ScopedKeys map[Identity]KeyResolver

func (keys ScopedKeys) ResolveResultKey(ctx context.Context, identity Identity, id string) (ed25519.PublicKey, error) {
	resolver := keys[identity]
	if resolver == nil {
		return nil, fmt.Errorf("unknown reporting Runner scope")
	}
	return resolver.ResolveResultKey(ctx, identity, id)
}

type Signer struct {
	Signer grant.Signer
	KeyID  string
}

func (s *Signer) Sign(ctx context.Context, identity Identity, hash string, result protocol.Result) (protocol.ResultReport, error) {
	if s == nil || s.Signer == nil {
		return protocol.ResultReport{}, fmt.Errorf("result signer unavailable")
	}
	if ctx.Err() != nil {
		return protocol.ResultReport{}, fmt.Errorf("result signing cancelled")
	}
	if result.Evidence != nil {
		evidence := *result.Evidence
		evidence.Window.From = evidence.Window.From.UTC()
		evidence.Window.To = evidence.Window.To.UTC()
		evidence.ObservedAt = evidence.ObservedAt.UTC()
		result.Evidence = &evidence
	}
	result.CompletedAt = result.CompletedAt.UTC()
	report := protocol.ResultReport{Version: protocol.ResultReportVersion, RunnerID: identity.RunnerID, TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup, RequestHash: hash, Result: result, Signature: protocol.Signature{KeyID: s.KeyID, Algorithm: grant.AlgorithmEd25519}}
	payload, err := protocol.CanonicalResultReportPayload(report)
	if err != nil {
		return protocol.ResultReport{}, err
	}
	signature, err := s.Signer.Sign(ctx, payload)
	if err != nil {
		return protocol.ResultReport{}, err
	}
	if ctx.Err() != nil {
		return protocol.ResultReport{}, fmt.Errorf("result signing cancelled")
	}
	if signature.KeyID != report.Signature.KeyID || signature.Algorithm != report.Signature.Algorithm || signature.Value == "" {
		return protocol.ResultReport{}, fmt.Errorf("result signer metadata mismatch")
	}
	report.Signature = signature
	return report, nil
}

type Verifier struct{ Keys KeyResolver }

func (v *Verifier) Verify(ctx context.Context, identity Identity, g protocol.ActionGrant, report protocol.ResultReport) error {
	if v == nil || v.Keys == nil {
		return fmt.Errorf("result verifier unavailable")
	}
	if identity != (Identity{report.RunnerID, report.TenantID, report.RunnerGroup}) || g.TenantID != identity.TenantID || g.RunnerGroup != identity.RunnerGroup || g.GrantID != report.Result.GrantID || g.RunID != report.Result.RunID || g.StepID != report.Result.StepID {
		return fmt.Errorf("result identity or grant binding mismatch")
	}
	hash, err := protocol.GrantHash(g)
	if err != nil || hash != report.RequestHash {
		return fmt.Errorf("result grant hash mismatch")
	}
	payload, err := protocol.CanonicalResultReportPayload(report)
	if err != nil {
		return err
	}
	key, err := v.Keys.ResolveResultKey(ctx, identity, report.Signature.KeyID)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(report.Signature.Value)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, payload, signature) {
		return fmt.Errorf("invalid result signature")
	}
	return nil
}
