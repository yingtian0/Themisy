package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/identity"
	"time"
)

// IdentityAttestor belongs to a separately deployed organization identity
// authority. The Grant dispatcher must not have access to its signing key.
type IdentityAttestor struct {
	Verifier *identity.AttestationVerifier
	Signer   grant.Signer
	KeyID    string
	Audit    IdentityAuditSink
}

func (a *IdentityAttestor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	fail := func() {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "IDENTITY_ATTESTATION_UNAVAILABLE"})
	}
	if a == nil || a.Verifier == nil || a.Signer == nil || a.Audit == nil || r.Method != "POST" {
		fail()
		return
	}
	actor, ok := identity.RequestActorFromContext(r.Context())
	if !ok || actor.TenantID != a.Verifier.TenantID || !slices.Contains(actor.Roles, "deployer") {
		fail()
		return
	}
	var input identity.AttestationRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.RequestID == "" || input.AgentID == "" || input.DelegationRef == "" {
		fail()
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	delegation, err := a.Verifier.Directory.ResolveDelegation(ctx, input.DelegationRef)
	if err != nil || delegation.UserSubject != actor.Subject || delegation.Issuer != actor.Issuer || delegation.AgentID != input.AgentID {
		fail()
		return
	}
	proof, err := identity.Attest(ctx, a.Verifier.OIDC, a.Signer, a.KeyID, identity.Attestation{Issuer: a.Verifier.Issuer, TenantID: actor.TenantID, Audience: a.Verifier.Audience, SubjectType: actor.SubjectType, RequestID: input.RequestID, AgentID: input.AgentID, DelegationRef: input.DelegationRef, Roles: actor.Roles}, actor.ProofToken)
	if err != nil {
		fail()
		return
	}
	receipt, err := a.Verifier.VerifyAttestation(ctx, proof)
	if err != nil {
		fail()
		return
	}
	now := time.Now().UTC()
	if a.Verifier.Now != nil {
		now = a.Verifier.Now().UTC()
	}
	event := domain.AuditEvent{ID: "identity_attestation_" + rand.Text(), CorrelationID: "identity/" + actor.TenantID + "/" + input.RequestID, ActorType: actor.SubjectType, ActorID: actor.Subject, Action: "identity.attest", ResourceType: "identity_decision", ResourceID: input.RequestID, Result: "VERIFIED", Timestamp: now, Details: map[string]any{"tenant_id": actor.TenantID, "issuer": actor.Issuer, "subject": actor.Subject, "delegation_ref": input.DelegationRef, "agent_id": input.AgentID, "expires_at": receipt.ExpiresAt, "reason_code": "INDEPENDENT_IDENTITY_ATTESTED"}}
	if a.Audit.AppendAuditContext(ctx, event) != nil || ctx.Err() != nil {
		fail()
		return
	}
	if _, err := a.Verifier.VerifyAttestation(ctx, proof); err != nil {
		fail()
		return
	}
	latest, err := a.Verifier.Directory.ResolveDelegation(ctx, input.DelegationRef)
	if err != nil || latest.UserSubject != actor.Subject || latest.Issuer != actor.Issuer || latest.AgentID != input.AgentID {
		fail()
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Proof string `json:"proof"`
	}{proof})
}
