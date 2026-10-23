package contextsnapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"themisy/adapters/datadog"
	"themisy/internal/domain"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

const DependencySchema = "themisy.dependency-context/v1alpha1"

var ErrDependencyContext = errors.New("DEPENDENCY_CONTEXT_UNAVAILABLE")

type DependencyKey struct{ TenantID, Service, Environment string }

// DependencyConfig is operator-owned configuration, never a request payload.
// Sets must explicitly list at least one dependency; absent sets cannot imply health.
type DependencyConfig struct {
	Sets   map[DependencyKey][]string
	Health datadog.HealthConfig
}
type DependencyRecord struct {
	SchemaVersion string                   `json:"schema_version"`
	TenantID      string                   `json:"tenant_id"`
	Target        adapter.Target           `json:"target"`
	CapturedAt    time.Time                `json:"captured_at"`
	Evidence      []datadog.HealthEvidence `json:"evidence"`
	Hash          string                   `json:"hash"`
}
type DependencySnapshot struct{ record DependencyRecord }

func (s DependencySnapshot) Record() DependencyRecord {
	r := s.record
	r.Evidence = slices.Clone(r.Evidence)
	return r
}
func (s DependencySnapshot) Hash() string { return s.record.Hash }
func (s *DependencySnapshot) UnmarshalJSON([]byte) error {
	if s != nil {
		*s = DependencySnapshot{}
	}
	return ErrDependencyContext
}
func dependencyHash(r DependencyRecord) (string, error) {
	r.Hash = ""
	b, e := json.Marshal(r)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

// RequireFreshDependencies checks this observation only, never execution authority.
func (s DependencySnapshot) RequireFreshDependencies(now time.Time) error {
	r := s.record
	h, e := dependencyHash(r)
	if e != nil || now.IsZero() || r.SchemaVersion != DependencySchema || r.Hash == "" || h != r.Hash || r.CapturedAt.IsZero() || r.CapturedAt.After(now) || len(r.Evidence) == 0 {
		return ErrDependencyContext
	}
	for _, v := range r.Evidence {
		if v.Status != adapter.VerificationPass || v.TenantID != r.TenantID || v.Target.Environment != r.Target.Environment || v.CapturedAt.IsZero() || v.CapturedAt.After(r.CapturedAt) || v.SourceUpdatedAt.IsZero() || v.SourceUpdatedAt.After(v.CapturedAt) || v.MaximumAge <= 0 || !v.SourceUpdatedAt.Add(v.MaximumAge).After(now) {
			return ErrDependencyContext
		}
	}
	return nil
}

type DependencyCollector struct {
	sets   map[DependencyKey][]string
	reader *datadog.HealthReader
	broker *credentials.DatadogBroker
	audit  CIAuditSink
	now    func() time.Time
}

func NewDependencyCollector(config DependencyConfig, source credentials.DatadogCredentialSource, client datadog.HTTPDoer, sink CIAuditSink) (*DependencyCollector, error) {
	if source == nil || sink == nil || len(config.Sets) == 0 {
		return nil, ErrDependencyContext
	}
	sets := make(map[DependencyKey][]string, len(config.Sets))
	for k, v := range config.Sets {
		if strings.TrimSpace(k.TenantID) == "" || strings.TrimSpace(k.Service) == "" || strings.TrimSpace(k.Environment) == "" || len(v) == 0 || len(v) > 128 {
			return nil, ErrDependencyContext
		}
		names := slices.Clone(v)
		slices.Sort(names)
		for i, name := range names {
			if strings.TrimSpace(name) == "" || (i > 0 && name == names[i-1]) {
				return nil, ErrDependencyContext
			}
			if _, ok := config.Health.Targets[datadog.HealthTargetKey{TenantID: k.TenantID, Service: name, Environment: k.Environment}]; !ok {
				return nil, ErrDependencyContext
			}
		}
		sets[k] = names
	}
	config.Health.Targets = maps.Clone(config.Health.Targets)
	reader, err := datadog.NewHealthReader(config.Health, client)
	if err != nil {
		return nil, ErrDependencyContext
	}
	allowed := make(map[credentials.DatadogTarget]struct{})
	for k, names := range sets {
		for _, name := range names {
			allowed[credentials.DatadogTarget{TenantID: k.TenantID, Service: name, Environment: k.Environment}] = struct{}{}
		}
	}
	c := &DependencyCollector{sets: sets, reader: reader, audit: sink, now: time.Now}
	c.broker = &credentials.DatadogBroker{Source: source, Allowed: allowed, Now: func() time.Time { return c.now().UTC() }}
	return c, nil
}

// Collect selects the entire dependency set from trusted configuration. Partial
// acquisition is discarded; provider four-valued observations remain inspectable.
// No snapshot escapes until the audit append commits. Audit records cannot restore trust.
func (c *DependencyCollector) Collect(ctx context.Context, key DependencyKey) (DependencySnapshot, error) {
	if c == nil || c.reader == nil || c.broker == nil || c.audit == nil || ctx.Err() != nil {
		return DependencySnapshot{}, ErrDependencyContext
	}
	names, ok := c.sets[key]
	if !ok {
		return DependencySnapshot{}, ErrDependencyContext
	}
	started := c.now().UTC()
	if started.IsZero() {
		return DependencySnapshot{}, ErrDependencyContext
	}
	r := DependencyRecord{SchemaVersion: DependencySchema, TenantID: key.TenantID, Target: adapter.Target{Service: key.Service, Environment: key.Environment}, Evidence: make([]datadog.HealthEvidence, 0, len(names))}
	for _, name := range names {
		cred, err := c.broker.Acquire(ctx, credentials.Request{Provider: credentials.DatadogProvider, TenantID: key.TenantID, Service: name, Environment: key.Environment, Purpose: credentials.PurposeVerify})
		if err != nil {
			return DependencySnapshot{}, ErrDependencyContext
		}
		evidence, err := c.reader.ReadHealth(ctx, datadog.HealthRequest{TenantID: key.TenantID, Target: adapter.Target{Service: name, Environment: key.Environment}}, cred)
		if err != nil || !evidence.Status.Valid() {
			return DependencySnapshot{}, ErrDependencyContext
		}
		r.Evidence = append(r.Evidence, evidence)
	}
	r.CapturedAt = c.now().UTC()
	if r.CapturedAt.Before(started) || ctx.Err() != nil {
		return DependencySnapshot{}, ErrDependencyContext
	}
	var err error
	r.Hash, err = dependencyHash(r)
	if err != nil {
		return DependencySnapshot{}, ErrDependencyContext
	}
	s := DependencySnapshot{record: r}
	id := "dependency_observation_" + rand.Text()
	// Each dependency retains its status; the aggregate never promotes unknowns.
	result := "INCONCLUSIVE"
	if s.RequireFreshDependencies(r.CapturedAt) == nil {
		result = "PASS"
	}
	event := domain.AuditEvent{ID: id, CorrelationID: "context/" + key.TenantID + "/" + id, ActorType: "system", ActorID: "dependency-collector", Action: "context.dependencies.observe", ResourceType: "dependency_snapshot", ResourceID: r.Hash, Result: result, Timestamp: r.CapturedAt, Details: map[string]any{"record": s.Record()}}
	appendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if c.audit.AppendAuditContext(appendCtx, event) != nil || appendCtx.Err() != nil || ctx.Err() != nil {
		return DependencySnapshot{}, ErrDependencyContext
	}
	return s, nil
}
