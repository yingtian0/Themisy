package runner

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"themisy/internal/identity"
	"themisy/pkg/protocol"
)

// ReceivedContexts binds context only after the transport has verified the
// Control Plane signature. It provides the existing Runner validation pipeline
// with an immutable view for the lifetime of one grant.
type ReceivedContexts struct {
	mu        sync.RWMutex
	contexts  map[string]PinnedContext
	approvals map[string][]string
}

func NewReceivedContexts() *ReceivedContexts {
	return &ReceivedContexts{contexts: make(map[string]PinnedContext), approvals: make(map[string][]string)}
}

func (r *ReceivedContexts) BindVerified(actionGrant protocol.ActionGrant) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contexts[actionGrant.RunID+"\x00"+actionGrant.StepID] = PinnedContext{PlanHash: actionGrant.PlanHash, ContractHash: actionGrant.ContractHash, ProfileHash: actionGrant.ProfileHash, PolicyHash: actionGrant.PolicyHash, EvidenceHash: actionGrant.EvidenceHash, ApprovalRequired: len(actionGrant.ApprovalProofs) > 0, Target: actionGrant.Target, Action: actionGrant.Action}
	r.approvals[actionGrant.GrantID] = append([]string(nil), actionGrant.ApprovalProofs...)
}

func (r *ReceivedContexts) ResolveContext(_ context.Context, runID, stepID string) (PinnedContext, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, ok := r.contexts[runID+"\x00"+stepID]
	if !ok {
		return PinnedContext{}, fmt.Errorf("verified grant context not found")
	}
	return value, nil
}

func (r *ReceivedContexts) VerifyApprovals(_ context.Context, actionGrant protocol.ActionGrant, _ identity.Subject) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	expected, ok := r.approvals[actionGrant.GrantID]
	if !ok || !slices.Equal(expected, actionGrant.ApprovalProofs) {
		return fmt.Errorf("approval proofs are not bound to the verified grant")
	}
	return nil
}
