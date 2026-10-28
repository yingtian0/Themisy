package application

import (
	"context"
	"fmt"
	"themisy/internal/contextsnapshot"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/pkg/protocol"
	"time"
)

type SourceAuthority struct {
	Source *contextsnapshot.FileAdmissionSource
	Now    func() time.Time
}

func (a *SourceAuthority) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}
func (a *SourceAuthority) read(ctx context.Context) (contextsnapshot.VerifiedAdmission, error) {
	if a == nil || a.Source == nil {
		return contextsnapshot.VerifiedAdmission{}, contextsnapshot.ErrSourceContext
	}
	return a.Source.Read(ctx)
}
func (a *SourceAuthority) planning(v contextsnapshot.VerifiedAdmission, policyHash string) (*planner.Planner, error) {
	b := v.Bundle()
	ttl := b.ExpiresAt.Sub(a.now())
	if ttl <= 0 {
		return nil, contextsnapshot.ErrSourceContext
	}
	if ttl > 15*time.Minute {
		ttl = 15 * time.Minute
	}
	return planner.NewFromContracts(v.Contracts(), v.Profiles(), planner.Options{Now: a.now, PlanTTL: ttl, PolicyHash: policyHash, EvidenceHash: b.Hash, ContextSnapshotRef: "signed-source:" + b.Hash})
}
func (a *SourceAuthority) Prepare(ctx context.Context, intent domain.ReleaseIntent, policyHashes ...string) (domain.ReleaseIntent, *planner.Planner, *domain.SourceContextBinding, error) {
	v, err := a.read(ctx)
	if err != nil {
		return intent, nil, nil, err
	}
	intent.Changes = append([]domain.Change(nil), intent.Changes...)
	b := v.Bundle()
	if intent.TenantID != b.TenantID {
		return intent, nil, nil, contextsnapshot.ErrSourceContext
	}
	binding := &domain.SourceContextBinding{Issuer: b.Issuer, Hash: b.Hash, Revision: b.Revision, ExpiresAt: b.ExpiresAt, Targets: map[string]domain.SourceArtifactBinding{}}
	for i, change := range intent.Changes {
		target, err := v.Target(change.Service, string(intent.Environment), change.CommitSHA, change.DesiredVersion)
		if err != nil {
			return intent, nil, nil, err
		}
		if _, duplicate := binding.Targets[change.Service]; duplicate {
			return intent, nil, nil, contextsnapshot.ErrSourceContext
		}
		intent.Changes[i].CommitSHA = target.Build.CommitSHA
		intent.Changes[i].CISuccess = true
		intent.Changes[i].DependenciesHealthy = true
		intent.Changes[i].DestructiveMigration = change.DestructiveMigration || target.Build.DestructiveMigration
		binding.Targets[change.Service] = domain.SourceArtifactBinding{CommitSHA: target.Build.CommitSHA, ArtifactDigest: target.Build.ArtifactDigest, DestructiveMigration: target.Build.DestructiveMigration}
	}
	hash := ""
	if len(policyHashes) > 0 {
		hash = policyHashes[0]
	}
	p, err := a.planning(v, hash)
	return intent, p, binding, err
}
func (a *SourceAuthority) VerifyPlan(ctx context.Context, plan domain.ReleasePlan, tenant string) (*planner.Planner, error) {
	if plan.Context == nil || plan.Context.Revision < 1 {
		return nil, contextsnapshot.ErrSourceContext
	}
	v, err := a.read(ctx)
	if err != nil {
		return nil, err
	}
	b := v.Bundle()
	binding := plan.Context
	if b.TenantID != tenant || binding.Issuer != b.Issuer || binding.Hash != b.Hash || plan.EvidenceHash != b.Hash || binding.Revision != b.Revision || !binding.ExpiresAt.Equal(b.ExpiresAt) {
		return nil, contextsnapshot.ErrSourceContext
	}
	count := 0
	for _, phase := range plan.Phases {
		for _, step := range phase.Steps {
			pinned, ok := binding.Targets[step.Service]
			if !ok || pinned.ArtifactDigest != step.DesiredVersion {
				return nil, contextsnapshot.ErrSourceContext
			}
			target, err := v.Target(step.Service, string(plan.Environment), pinned.CommitSHA, pinned.ArtifactDigest)
			if err != nil || target.Build.DestructiveMigration != pinned.DestructiveMigration || target.Source.Metadata.Contract.ContentHash != step.ContractHash || target.Source.Metadata.Profile.ContentHash != step.ProfileHash {
				return nil, contextsnapshot.ErrSourceContext
			}
			count++
		}
	}
	if count != len(binding.Targets) {
		return nil, contextsnapshot.ErrSourceContext
	}
	p, err := a.planning(v, plan.PolicyHash)
	if err != nil {
		return nil, err
	}
	if err := p.ValidatePlan(plan, a.now()); err != nil {
		return nil, err
	}
	return p, nil
}
func (a *SourceAuthority) Authorize(ctx context.Context, run domain.ReleaseRun) (time.Time, error) {
	if _, err := a.VerifyPlan(ctx, run.Plan, run.TenantID); err != nil {
		return time.Time{}, err
	}
	return run.Plan.Context.ExpiresAt, nil
}
func (a *SourceAuthority) VerifyGrant(ctx context.Context, g protocol.ActionGrant) error {
	if g.ContextRevision < 1 || g.ContextIssuer == "" || g.ContextExpiresAt == "" {
		return contextsnapshot.ErrSourceContext
	}
	v, err := a.read(ctx)
	if err != nil {
		return err
	}
	b := v.Bundle()
	if b.TenantID != g.TenantID || b.Issuer != g.ContextIssuer || b.Hash != g.EvidenceHash || b.Revision != g.ContextRevision || b.ExpiresAt.Format(time.RFC3339Nano) != g.ContextExpiresAt || g.ExpiresAt.After(b.ExpiresAt) {
		return contextsnapshot.ErrSourceContext
	}
	target, err := v.Target(g.Target.Service, g.Target.Environment, "", g.Action.ArtifactDigest)
	if err != nil || target.Source.Metadata.Contract.ContentHash != g.ContractHash || target.Source.Metadata.Profile.ContentHash != g.ProfileHash {
		return contextsnapshot.ErrSourceContext
	}
	for _, c := range v.Contracts() {
		if c.Metadata.Name == g.Target.Service && c.Environments[g.Target.Environment].RunnerGroup == g.RunnerGroup {
			return nil
		}
	}
	return fmt.Errorf("SOURCE_RUNNER_GROUP_MISMATCH")
}
func (a *SourceAuthority) Services() []domain.Service {
	v, err := a.read(context.Background())
	if err != nil {
		return []domain.Service{}
	}
	p, err := a.planning(v, "")
	if err != nil {
		return []domain.Service{}
	}
	return p.Services()
}
