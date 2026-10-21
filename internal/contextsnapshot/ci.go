// Package contextsnapshot collects read-only source observations. A CI snapshot
// is not a complete pre-dispatch Context or permission to execute an action.
package contextsnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"strings"
	"time"

	"themisy/adapters/githubactions"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

const CISchema = "themisy.ci-context/v1alpha1"

var (
	ErrCIContextUnavailable = errors.New("CI_CONTEXT_UNAVAILABLE")
	ErrUntrustedCIRecord    = errors.New("CI_RECORD_REQUIRES_TRUSTED_RECOLLECTION")
	commitSHA               = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// CIRecord is exportable observation data, not authenticated input. Its hash
// detects accidental changes; it is not a signature or a deployment proof.
type CIRecord struct {
	SchemaVersion string                   `json:"schema_version"`
	TenantID      string                   `json:"tenant_id"`
	Target        adapter.Target           `json:"target"`
	CommitSHA     string                   `json:"commit_sha"`
	CapturedAt    time.Time                `json:"captured_at"`
	Evidence      githubactions.CIEvidence `json:"evidence"`
	Hash          string                   `json:"hash"`
}

// CISnapshot can be created only by this package's collector. Exported records
// must not be deserialized into a trusted value or used as a positive cache.
type CISnapshot struct{ record CIRecord }

func (s CISnapshot) Record() CIRecord {
	r := s.record
	r.Evidence.Artifacts = append([]githubactions.CIArtifact(nil), r.Evidence.Artifacts...)
	return r
}

func (s CISnapshot) Hash() string { return s.record.Hash }

func (s *CISnapshot) UnmarshalJSON([]byte) error {
	if s != nil {
		*s = CISnapshot{}
	}
	return ErrUntrustedCIRecord
}

// RequireFreshCI checks only this CI observation. It says nothing about runtime
// health, artifact/image provenance, identity, policy, or Grant authorization.
func (s CISnapshot) RequireFreshCI(now time.Time) error {
	r := s.record
	e := r.Evidence
	hash, err := hashRecord(r)
	if err != nil || now.IsZero() || r.SchemaVersion != CISchema || r.Hash == "" || r.Hash != hash ||
		e.Status != adapter.VerificationPass || e.SourceUpdatedAt.IsZero() ||
		e.MaximumAge <= 0 || r.CapturedAt.IsZero() || r.CapturedAt.After(now) ||
		e.ObservedAt.IsZero() || e.ObservedAt.After(r.CapturedAt) || e.SourceUpdatedAt.After(e.ObservedAt) ||
		!e.SourceUpdatedAt.Add(e.MaximumAge).After(now) {
		return ErrCIContextUnavailable
	}
	return nil
}

type CICollector struct {
	client  *githubactions.CIAdapter
	broker  *credentials.GitHubBroker
	targets map[githubactions.CITargetKey]githubactions.CITarget
	now     func() time.Time
}

// NewCICollector snapshots operator allowlists. The token source is a trusted
// customer-side read credential source; no credential enters exported records.
func NewCICollector(config githubactions.CIConfig, tokenSource credentials.GitHubTokenSource, client githubactions.HTTPDoer) (*CICollector, error) {
	if tokenSource == nil {
		return nil, ErrCIContextUnavailable
	}
	config.Targets = maps.Clone(config.Targets)
	reader, err := githubactions.NewCIAdapter(config, client)
	if err != nil {
		return nil, ErrCIContextUnavailable
	}
	allowed := make(map[credentials.GitHubTarget]struct{}, len(config.Targets))
	for key, target := range config.Targets {
		if target.TenantID != key.TenantID {
			return nil, ErrCIContextUnavailable
		}
		allowed[credentials.GitHubTarget{TenantID: key.TenantID, Service: key.Service, Environment: key.Environment}] = struct{}{}
	}
	c := &CICollector{client: reader, targets: config.Targets, now: time.Now}
	clock := func() time.Time { return c.now().UTC() }
	c.client.Now = clock
	c.broker = &credentials.GitHubBroker{Source: tokenSource, Allowed: allowed, Now: clock}
	return c, nil
}

// Collect has no caller success/health flag, repository, workflow, URL or
// credential argument. Provider statuses stay four-valued in the snapshot.
func (c *CICollector) Collect(ctx context.Context, request githubactions.CIRequest) (CISnapshot, error) {
	if c == nil || c.client == nil || c.broker == nil || ctx.Err() != nil {
		return CISnapshot{}, ErrCIContextUnavailable
	}
	request.CommitSHA = strings.ToLower(request.CommitSHA)
	if !commitSHA.MatchString(request.CommitSHA) {
		return CISnapshot{}, ErrCIContextUnavailable
	}
	if _, ok := c.targets[githubactions.CITargetKey{TenantID: request.TenantID, Service: request.Target.Service, Environment: request.Target.Environment}]; !ok {
		return CISnapshot{}, ErrCIContextUnavailable
	}
	credential, err := c.broker.Acquire(ctx, credentials.Request{Provider: credentials.GitHubProvider, TenantID: request.TenantID, Service: request.Target.Service, Environment: request.Target.Environment, Purpose: credentials.PurposeVerify})
	if err != nil {
		return CISnapshot{}, ErrCIContextUnavailable
	}
	evidence, err := c.client.ReadCI(ctx, request, credential)
	if err != nil || ctx.Err() != nil || !evidence.Status.Valid() {
		return CISnapshot{}, ErrCIContextUnavailable
	}
	r := CIRecord{SchemaVersion: CISchema, TenantID: request.TenantID, Target: request.Target, CommitSHA: request.CommitSHA, CapturedAt: c.now().UTC(), Evidence: evidence}
	r.Hash, err = hashRecord(r)
	if err != nil {
		return CISnapshot{}, ErrCIContextUnavailable
	}
	return CISnapshot{record: r}, nil
}

func hashRecord(record CIRecord) (string, error) {
	record.Hash = ""
	payload, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
