package contextsnapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"themisy/internal/domain"
	"themisy/pkg/adapter"
	"time"
)

const SourceContextSchema = "themisy.source-context/v1alpha1"

var ErrSourceContext = errors.New("SOURCE_CONTEXT_UNAVAILABLE")

// SourceContextRecord binds the covered source observations, not a complete
// authorization context, artifact provenance or permission to issue a Grant.
type SourceContextRecord struct {
	SchemaVersion      string            `json:"schema_version"`
	Mode               string            `json:"mode"`
	TenantID           string            `json:"tenant_id"`
	Target             adapter.Target    `json:"target"`
	CommitSHA          string            `json:"commit_sha"`
	DependencyServices []string          `json:"dependency_services"`
	CapturedAt         time.Time         `json:"captured_at"`
	ExpiresAt          time.Time         `json:"expires_at"`
	CI                 CIRecord          `json:"ci"`
	Metadata           MetadataRecord    `json:"metadata"`
	Dependencies       *DependencyRecord `json:"dependencies,omitempty"`
	Hash               string            `json:"hash"`
}
type SourceContextSnapshot struct {
	record       SourceContextRecord
	ci           CISnapshot
	metadata     MetadataSnapshot
	dependencies DependencySnapshot
}

func (s SourceContextSnapshot) Hash() string { return s.record.Hash }
func (s SourceContextSnapshot) Record() SourceContextRecord {
	r := s.record
	r.DependencyServices = slices.Clone(r.DependencyServices)
	r.CI = s.ci.Record()
	r.Metadata = s.metadata.Record()
	if s.record.Dependencies != nil {
		d := s.dependencies.Record()
		r.Dependencies = &d
	}
	return r
}
func (s *SourceContextSnapshot) UnmarshalJSON([]byte) error {
	if s != nil {
		*s = SourceContextSnapshot{}
	}
	return ErrSourceContext
}
func sourceContextHash(r SourceContextRecord) (string, error) {
	r.Hash = ""
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func (s SourceContextSnapshot) RequireFreshSources(now time.Time) error {
	r := s.Record()
	hash, err := sourceContextHash(r)
	if err != nil || r.SchemaVersion != SourceContextSchema || r.Hash == "" || hash != r.Hash || now.IsZero() || r.CapturedAt.IsZero() || r.CapturedAt.After(now) || !r.ExpiresAt.After(now) || s.ci.RequireFreshCI(now) != nil || s.metadata.RequireFreshMetadata(now) != nil {
		return ErrSourceContext
	}
	if r.Dependencies != nil && s.dependencies.RequireFreshDependencies(now) != nil {
		return ErrSourceContext
	}
	return nil
}

type SourceContextAssembler struct {
	sink CIAuditSink
	now  func() time.Time
}

func NewSourceContextAssembler(sink CIAuditSink) (*SourceContextAssembler, error) {
	if sink == nil {
		return nil, ErrSourceContext
	}
	return &SourceContextAssembler{sink: sink, now: time.Now}, nil
}

// Assemble accepts only opaque snapshots minted by trusted collectors. Scope,
// application repository and the Contract's complete service-dependency set must
// agree. Metadata source revision and application commit are distinct concepts.
func (a *SourceContextAssembler) Assemble(ctx context.Context, ci CISnapshot, dependencies DependencySnapshot, metadata MetadataSnapshot) (SourceContextSnapshot, error) {
	if a == nil || a.sink == nil || ctx.Err() != nil {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	now := a.now().UTC()
	if now.IsZero() || ci.RequireFreshCI(now) != nil || metadata.RequireFreshMetadata(now) != nil {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	c, m := ci.Record(), metadata.Record()
	if c.TenantID != m.TenantID || c.Target.Service != m.Service || c.Target.Environment != m.Environment || !commitSHA.MatchString(c.CommitSHA) || c.CommitSHA != c.Evidence.CommitSHA {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	contract, err := metadata.Contract()
	if err != nil {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	repository := c.Evidence.Repository
	if repository == "" || (contract.Metadata.Repository != repository && contract.Metadata.Repository != "github.com/"+repository && contract.Metadata.Repository != "https://github.com/"+repository) {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	names := []string{}
	seen := map[string]bool{}
	for _, dependency := range contract.Dependencies {
		if dependency.Service != "" {
			if dependency.Service == c.Target.Service || strings.TrimSpace(dependency.Service) != dependency.Service {
				return SourceContextSnapshot{}, ErrSourceContext
			}
			if !seen[dependency.Service] {
				seen[dependency.Service] = true
				names = append(names, dependency.Service)
			}
		}
	}
	slices.Sort(names)
	expires := c.Evidence.SourceUpdatedAt.Add(c.Evidence.MaximumAge)
	for _, item := range []MetadataItem{m.Contract, m.Profile} {
		if until := item.FetchedAt.Add(item.MaximumAge); until.Before(expires) {
			expires = until
		}
	}
	r := SourceContextRecord{SchemaVersion: SourceContextSchema, Mode: "SOURCE_OBSERVATIONS_ONLY", TenantID: c.TenantID, Target: c.Target, CommitSHA: c.CommitSHA, DependencyServices: names, CapturedAt: now, ExpiresAt: expires, CI: c, Metadata: m}
	if len(names) > 0 {
		if dependencies.RequireFreshDependencies(now) != nil {
			return SourceContextSnapshot{}, ErrSourceContext
		}
		d := dependencies.Record()
		if d.TenantID != c.TenantID || d.Target != c.Target || len(d.Evidence) != len(names) {
			return SourceContextSnapshot{}, ErrSourceContext
		}
		actual := make([]string, 0, len(d.Evidence))
		for _, evidence := range d.Evidence {
			actual = append(actual, evidence.Target.Service)
			if until := evidence.SourceUpdatedAt.Add(evidence.MaximumAge); until.Before(r.ExpiresAt) {
				r.ExpiresAt = until
			}
		}
		slices.Sort(actual)
		if !slices.Equal(names, actual) {
			return SourceContextSnapshot{}, ErrSourceContext
		}
		r.Dependencies = &d
	} else if dependencies.Hash() != "" {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	r.Hash, err = sourceContextHash(r)
	if err != nil {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	snapshot := SourceContextSnapshot{record: r, ci: ci, metadata: metadata, dependencies: dependencies}
	if snapshot.RequireFreshSources(now) != nil {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	id := "source_context_" + rand.Text()
	// Record deep-copies artifact/dependency slices before passing to the sink.
	event := domain.AuditEvent{ID: id, CorrelationID: "context/" + c.TenantID + "/" + id, ActorType: "system", ActorID: "source-context-assembler", Action: "context.sources.observe", ResourceType: "source_context_snapshot", ResourceID: r.Hash, Result: "OBSERVED", Timestamp: now, Details: map[string]any{"record": snapshot.Record()}}
	appendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if a.sink.AppendAuditContext(appendCtx, event) != nil || appendCtx.Err() != nil || ctx.Err() != nil || snapshot.RequireFreshSources(a.now().UTC()) != nil {
		return SourceContextSnapshot{}, ErrSourceContext
	}
	return snapshot, nil
}
