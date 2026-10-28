package policy

import (
	"context"
	"crypto/rand"
	"themisy/internal/domain"
	"themisy/internal/policymodel"
	"time"
)

var ErrDistribution = policymodel.ErrDistribution
var ErrAssignmentConflict = policymodel.ErrAssignmentConflict

type Assignment = policymodel.Assignment
type Delivery = policymodel.Delivery
type Acknowledgement = policymodel.Acknowledgement
type DistributionStore interface {
	RegisterPolicyBundle(context.Context, string, Bundle, domain.AuditEvent) error
	GetPolicyBundle(context.Context, string, string) (Bundle, error)
	GetPolicyAssignment(context.Context, string, string) (Assignment, error)
	SetPolicyAssignment(context.Context, Assignment, int64, domain.AuditEvent) (Assignment, error)
	AcknowledgePolicy(context.Context, string, string, Acknowledgement, domain.AuditEvent) error
	PolicyAcknowledgements(context.Context, string, string, int64) ([]Acknowledgement, error)
}
type RevisionLedger interface {
	AcceptPolicyRevision(context.Context, Assignment) error
}
type Distributor struct {
	Authorize        func(context.Context, string, string, string) error
	TenantID, Issuer string
	Keys             BundleKeyResolver
	Store            DistributionStore
	AllowedGroups    map[string]bool
	Now              func() time.Time
}

func (d *Distributor) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}
func (d *Distributor) scope(tenant, group string) bool {
	return d != nil && d.Store != nil && d.Keys != nil && tenant == d.TenantID && tenant != "" && (group == "" || d.AllowedGroups[group])
}
func (d *Distributor) validate(ctx context.Context, b Bundle) error {
	if !d.scope(d.TenantID, "") || ctx.Err() != nil {
		return ErrDistribution
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := NewVerifiedOPA(bounded, b, d.Issuer, d.Keys, d.now()); err != nil || bounded.Err() != nil {
		return ErrDistribution
	}
	if VerifyBundle(bounded, b, d.Issuer, d.Keys, d.now()) != nil {
		return ErrDistribution
	}
	return nil
}
func (d *Distributor) event(actor, issuer, action, resource string, details map[string]any) domain.AuditEvent {
	details["tenant_id"] = d.TenantID
	details["actor_issuer"] = issuer
	return domain.AuditEvent{ID: "policy_" + rand.Text(), CorrelationID: "policy/" + d.TenantID, ActorType: "user", ActorID: actor, Action: action, ResourceType: "policy", ResourceID: resource, Result: "ACCEPTED", Timestamp: d.now(), Details: details}
}
func (d *Distributor) Register(ctx context.Context, tenant, actor, actorIssuer string, b Bundle) (string, error) {
	if !d.scope(tenant, "") || actor == "" || actorIssuer == "" || d.validate(ctx, b) != nil {
		return "", ErrDistribution
	}
	if d.Authorize != nil && d.Authorize(ctx, tenant, actor, actorIssuer) != nil {
		return "", ErrDistribution
	}
	if err := d.Store.RegisterPolicyBundle(ctx, tenant, b, d.event(actor, actorIssuer, "policy.register", b.Hash, map[string]any{"bundle_hash": b.Hash, "version": b.Version, "issuer": b.Issuer})); err != nil {
		return "", err
	}
	return b.Hash, nil
}
func (d *Distributor) Bundle(ctx context.Context, tenant, hash string) (Bundle, error) {
	if !d.scope(tenant, "") || !candidateHash.MatchString(hash) {
		return Bundle{}, ErrDistribution
	}
	bundle, err := d.Store.GetPolicyBundle(ctx, tenant, hash)
	if err != nil || bundle.Hash != hash || d.validate(ctx, bundle) != nil {
		return Bundle{}, ErrDistribution
	}
	return bundle, nil
}
func (d *Distributor) Current(ctx context.Context, tenant, group string) (Delivery, error) {
	if !d.scope(tenant, group) || group == "" {
		return Delivery{}, ErrDistribution
	}
	assignment, err := d.Store.GetPolicyAssignment(ctx, tenant, group)
	if err != nil || assignment.TenantID != tenant || assignment.RunnerGroup != group || assignment.Revision < 1 {
		return Delivery{}, ErrDistribution
	}
	bundle, err := d.Bundle(ctx, tenant, assignment.BundleHash)
	if err != nil {
		return Delivery{}, err
	}
	return Delivery{Assignment: assignment, Bundle: bundle}, nil
}
func (d *Distributor) Assign(ctx context.Context, tenant, group, hash, actor, actorIssuer string, expected int64) (Assignment, error) {
	if !d.scope(tenant, group) || group == "" || expected < 0 || actor == "" || actorIssuer == "" {
		return Assignment{}, ErrDistribution
	}
	if _, err := d.Bundle(ctx, tenant, hash); err != nil {
		return Assignment{}, err
	}
	if d.Authorize != nil && d.Authorize(ctx, tenant, actor, actorIssuer) != nil {
		return Assignment{}, ErrDistribution
	}
	return d.Store.SetPolicyAssignment(ctx, Assignment{TenantID: tenant, RunnerGroup: group, Revision: expected + 1, BundleHash: hash, UpdatedAt: d.now()}, expected, d.event(actor, actorIssuer, "policy.assign", group, map[string]any{"bundle_hash": hash, "expected_revision": expected, "runner_group": group}))
}
func (d *Distributor) Rollback(ctx context.Context, tenant, group, actor, actorIssuer string, expected int64) (Assignment, error) {
	if !d.scope(tenant, group) || group == "" {
		return Assignment{}, ErrDistribution
	}
	current, err := d.Store.GetPolicyAssignment(ctx, tenant, group)
	if err != nil || current.Revision != expected || current.PreviousHash == "" {
		return Assignment{}, ErrAssignmentConflict
	}
	// A rollback advances the revision and must reverify the previous signature
	// and lifetime. It cannot reactivate an expired or revoked bundle.
	return d.Assign(ctx, tenant, group, current.PreviousHash, actor, actorIssuer, expected)
}
func (d *Distributor) Acknowledge(ctx context.Context, tenant, group, runner string, revision int64, hash string) error {
	current, err := d.Current(ctx, tenant, group)
	if err != nil || runner == "" || current.Assignment.Revision != revision || current.Assignment.BundleHash != hash {
		return ErrAssignmentConflict
	}
	ack := Acknowledgement{RunnerID: runner, Revision: revision, BundleHash: hash, ObservedAt: d.now()}
	event := d.event(runner, "workload-transport", "policy.acknowledge", group, map[string]any{"revision": revision, "bundle_hash": hash, "runner_group": group})
	event.ActorType = "runner"
	return d.Store.AcknowledgePolicy(ctx, tenant, group, ack, event)
}

// AssignedEvaluator is used by Control Plane and Runner with trusted, fresh
// sources. It never falls back to an offline candidate or an embedded-only hash.
type AssignedEvaluator struct {
	Load   func(context.Context, string, string) (Delivery, error)
	Issuer string
	Keys   BundleKeyResolver
	Now    func() time.Time
}

func (e *AssignedEvaluator) load(ctx context.Context, input Input) (Delivery, error) {
	if e == nil || e.Load == nil || e.Keys == nil {
		return Delivery{}, ErrDistribution
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	delivery, err := e.Load(bounded, input.TenantID, input.RunnerGroup)
	if err != nil || delivery.Assignment.TenantID != input.TenantID || delivery.Assignment.RunnerGroup != input.RunnerGroup || delivery.Assignment.Revision < 1 || delivery.Assignment.BundleHash != delivery.Bundle.Hash {
		return Delivery{}, ErrDistribution
	}
	return delivery, nil
}
func (e *AssignedEvaluator) PolicyHashFor(ctx context.Context, input Input) (string, error) {
	d, err := e.load(ctx, input)
	if err != nil {
		return "", err
	}
	return d.Bundle.Hash, nil
}
func (e *AssignedEvaluator) Evaluate(ctx context.Context, input Input) (Evaluation, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	delivery, err := e.load(bounded, input)
	if err != nil || input.PolicyHash != delivery.Bundle.Hash || input.PolicyRevision != delivery.Assignment.Revision {
		return Evaluation{}, ErrDistribution
	}
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	opa, err := NewVerifiedOPA(bounded, delivery.Bundle, e.Issuer, e.Keys, now)
	if err != nil {
		return Evaluation{}, err
	}
	result, err := opa.Evaluate(bounded, input)
	if err != nil {
		return Evaluation{}, err
	}
	now = time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	if VerifyBundle(bounded, delivery.Bundle, e.Issuer, e.Keys, now) != nil || bounded.Err() != nil {
		return Evaluation{}, ErrDistribution
	}
	return result, nil
}
