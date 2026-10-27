package verification

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"themisy/adapters/ecs"
	"themisy/internal/domain"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"time"
)

var ErrRuntimeAudit = errors.New("RUNTIME_OBSERVATION_AUDIT_UNAVAILABLE")

const RuntimeRecordSchema = "themisy.runtime-context/v1alpha1"

type RuntimeAuditSink interface {
	AppendAuditContext(context.Context, domain.AuditEvent) error
}
type RuntimeAuditRequest struct {
	RunID, GrantID string
	Request        adapter.ReconcileRequest
}
type RuntimeRecord struct {
	SchemaVersion string                 `json:"schema_version"`
	TenantID      string                 `json:"tenant_id"`
	RunID         string                 `json:"run_id"`
	GrantID       string                 `json:"grant_id"`
	CapturedAt    time.Time              `json:"captured_at"`
	MaximumAge    time.Duration          `json:"maximum_age"`
	Observation   ecs.RuntimeObservation `json:"observation"`
	Hash          string                 `json:"hash"`
}
type RuntimeSnapshot struct{ record RuntimeRecord }

func (s RuntimeSnapshot) Record() RuntimeRecord {
	r := s.record
	r.Observation = r.Observation.Clone()
	return r
}
func (s RuntimeSnapshot) Hash() string { return s.record.Hash }
func (s *RuntimeSnapshot) UnmarshalJSON([]byte) error {
	if s != nil {
		*s = RuntimeSnapshot{}
	}
	return ErrRuntimeAudit
}
func runtimeRecordHash(r RuntimeRecord) (string, error) {
	r.Hash = ""
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func (s RuntimeSnapshot) RequireFreshRuntime(now time.Time) error {
	r := s.record
	hash, err := runtimeRecordHash(r)
	observationHash, observationErr := ecs.RuntimeObservationHash(r.Observation)
	if err != nil || observationErr != nil || hash != r.Hash || hash == "" || r.SchemaVersion != RuntimeRecordSchema || r.Observation.Hash != observationHash || r.Observation.Status != adapter.VerificationPass || now.IsZero() || r.CapturedAt.After(now) || r.Observation.ObservedAt.IsZero() || r.Observation.ObservedAt.After(r.CapturedAt) || r.MaximumAge <= 0 || !r.Observation.ObservedAt.Add(r.MaximumAge).After(now) {
		return ErrRuntimeAudit
	}
	return nil
}

type RuntimeCollector struct {
	tenant     string
	reader     *ecs.Adapter
	broker     credentials.Broker
	sink       RuntimeAuditSink
	maximumAge time.Duration
	now        func() time.Time
}

func NewRuntimeCollector(tenant string, reader *ecs.Adapter, broker credentials.Broker, sink RuntimeAuditSink, maximumAge time.Duration) (*RuntimeCollector, error) {
	if tenant == "" || strings.TrimSpace(tenant) != tenant || reader == nil || broker == nil || sink == nil || maximumAge < time.Second || maximumAge > 5*time.Minute {
		return nil, ErrRuntimeAudit
	}
	return &RuntimeCollector{tenant: tenant, reader: reader, broker: broker, sink: sink, maximumAge: maximumAge, now: time.Now}, nil
}

// Collect correlates observations; caller-supplied Run/Grant IDs confer no trust
// or execution authority. Only a configured read credential path is exercised.
func (c *RuntimeCollector) Collect(ctx context.Context, r RuntimeAuditRequest) (RuntimeSnapshot, error) {
	if c == nil || c.reader == nil || c.broker == nil || c.sink == nil || ctx.Err() != nil || r.RunID == "" || r.GrantID == "" || c.reader.AuthorizeRuntime(r.Request) != nil {
		return RuntimeSnapshot{}, ErrRuntimeAudit
	}
	started := c.now().UTC()
	if started.IsZero() {
		return RuntimeSnapshot{}, ErrRuntimeAudit
	}
	credential, err := c.broker.Acquire(ctx, credentials.Request{Provider: credentials.ProviderAWS, TenantID: c.tenant, RunID: r.RunID, GrantID: r.GrantID, Service: r.Request.Target.Service, Environment: r.Request.Target.Environment, Purpose: credentials.PurposeVerify})
	if err != nil {
		return RuntimeSnapshot{}, ErrRuntimeAudit
	}
	observation, err := c.reader.ReadRuntime(ctx, r.Request, credential)
	if err != nil || ctx.Err() != nil {
		return RuntimeSnapshot{}, ErrRuntimeAudit
	}
	record := RuntimeRecord{SchemaVersion: RuntimeRecordSchema, TenantID: c.tenant, RunID: r.RunID, GrantID: r.GrantID, CapturedAt: c.now().UTC(), MaximumAge: c.maximumAge, Observation: observation}
	if record.CapturedAt.Before(started) || observation.ObservedAt.After(record.CapturedAt) || !observation.ObservedAt.Add(c.maximumAge).After(record.CapturedAt) {
		return RuntimeSnapshot{}, ErrRuntimeAudit
	}
	record.Hash, err = runtimeRecordHash(record)
	if err != nil {
		return RuntimeSnapshot{}, ErrRuntimeAudit
	}
	id := "runtime_observation_" + rand.Text()
	event := domain.AuditEvent{ID: id, CorrelationID: "runtime/" + c.tenant + "/" + r.RunID + "/" + id, ActorType: "system", ActorID: "runtime-collector", Action: "runtime.observe", ResourceType: "runtime_snapshot", ResourceID: record.Hash, Result: string(observation.Status), Timestamp: record.CapturedAt, Details: map[string]any{"record": func() RuntimeRecord { owned := record; owned.Observation = owned.Observation.Clone(); return owned }()}}
	appendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if c.sink.AppendAuditContext(appendCtx, event) != nil || appendCtx.Err() != nil || ctx.Err() != nil {
		return RuntimeSnapshot{}, ErrRuntimeAudit
	}
	at := c.now().UTC()
	if at.Before(record.CapturedAt) || !observation.ObservedAt.Add(c.maximumAge).After(at) {
		return RuntimeSnapshot{}, ErrRuntimeAudit
	}
	return RuntimeSnapshot{record: record}, nil
}
