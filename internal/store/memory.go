package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"themisy/internal/domain"
	"themisy/pkg/protocol"
)

type Memory struct {
	manifests      map[string]protocol.PlanManifest
	mu             sync.RWMutex
	runs           map[string]*domain.ReleaseRun
	plans          map[string]domain.ReleasePlan
	runByRequestID map[string]string
	audit          map[string][]domain.AuditEvent
	executions     map[string]ExecutionRecord
	outbox         []domain.OutboxEvent
	projections    map[string]*domain.ReleaseRun
	runnerActions  map[string]RunnerActionRecord
	runnerByKey    map[string]string
	grantDispatch  map[string]GrantDispatchRecord
	grantByKey     map[string]string
	runnerFleet    map[string]domain.RunnerInfo
	journalError   error
}

func NewMemory() *Memory {
	return &Memory{
		runs: make(map[string]*domain.ReleaseRun), plans: make(map[string]domain.ReleasePlan), runByRequestID: make(map[string]string),
		audit: make(map[string][]domain.AuditEvent), executions: make(map[string]ExecutionRecord),
		projections:   make(map[string]*domain.ReleaseRun),
		runnerActions: make(map[string]RunnerActionRecord), runnerByKey: make(map[string]string),
		grantDispatch: make(map[string]GrantDispatchRecord), grantByKey: make(map[string]string),
		runnerFleet: make(map[string]domain.RunnerInfo),
	}
}

func (m *Memory) HeartbeatRunner(_ context.Context, heartbeat domain.RunnerInfo, audit domain.AuditEvent) (domain.RunnerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if heartbeat.ID == "" || heartbeat.TenantID == "" || heartbeat.Group == "" || heartbeat.ReportedCapacity < 0 || heartbeat.InFlight < 0 || heartbeat.InFlight > heartbeat.ReportedCapacity || heartbeat.LastSeen.IsZero() {
		return domain.RunnerInfo{}, ErrConflict
	}
	key := runnerFleetKey(heartbeat.TenantID, heartbeat.ID)
	current, exists := m.runnerFleet[key]
	if exists && current.Group != heartbeat.Group {
		return domain.RunnerInfo{}, ErrConflict
	}
	if !exists {
		current = heartbeat
		current.Status = domain.RunnerReady
		current.StateVersion = 1
	} else {
		current.ReportedCapacity = heartbeat.ReportedCapacity
		current.InFlight = heartbeat.InFlight
		current.LastSeen = heartbeat.LastSeen
		current.StateVersion++
	}
	current.Capacity = current.ReportedCapacity - current.InFlight
	if current.Status != domain.RunnerReady {
		current.Capacity = 0
	}
	m.runnerFleet[key] = current
	if audit.ID != "" {
		m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], audit)
	}
	return current, nil
}

func (m *Memory) GetRunner(_ context.Context, tenantID, runnerID string) (domain.RunnerInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.runnerFleet[runnerFleetKey(tenantID, runnerID)]
	if !ok {
		return domain.RunnerInfo{}, ErrNotFound
	}
	return value, nil
}

func (m *Memory) ListRunnerFleet(_ context.Context, tenantID string) ([]domain.RunnerInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := []domain.RunnerInfo{}
	for _, value := range m.runnerFleet {
		if value.TenantID == tenantID {
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (m *Memory) SetRunnerStatus(_ context.Context, tenantID, runnerID string, status domain.RunnerStatus, actor string, now time.Time, audit domain.AuditEvent) (domain.RunnerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if actor == "" || (status != domain.RunnerReady && status != domain.RunnerDraining && status != domain.RunnerFrozen) {
		return domain.RunnerInfo{}, ErrConflict
	}
	key := runnerFleetKey(tenantID, runnerID)
	current, ok := m.runnerFleet[key]
	if !ok {
		return domain.RunnerInfo{}, ErrNotFound
	}
	current.Status, current.ControlledBy, current.ControlledAt = status, actor, &now
	current.Capacity = current.ReportedCapacity - current.InFlight
	if status != domain.RunnerReady {
		current.Capacity = 0
	}
	if status == domain.RunnerFrozen {
		current.FrozenBy, current.FrozenAt = actor, &now
	} else {
		current.FrozenBy, current.FrozenAt = "", nil
	}
	current.StateVersion++
	m.runnerFleet[key] = current
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], audit)
	return current, nil
}

func (m *Memory) CreateGrantDispatch(_ context.Context, record GrantDispatchRecord, audit domain.AuditEvent, outbox domain.OutboxEvent) (GrantDispatchRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := runnerIdempotencyKey(record.Grant.TenantID, record.Grant.RunnerGroup, record.Grant.IdempotencyKey)
	if id, ok := m.grantByKey[key]; ok {
		return cloneGrantDispatch(m.grantDispatch[id]), false, nil
	}
	if _, ok := m.grantDispatch[record.Grant.GrantID]; ok {
		return GrantDispatchRecord{}, false, fmt.Errorf("duplicate grant id %q", record.Grant.GrantID)
	}
	record.Status, record.StateVersion, record.OutboxID = GrantDispatchPending, 1, outbox.ID
	m.grantDispatch[record.Grant.GrantID] = cloneGrantDispatch(record)
	m.grantByKey[key] = record.Grant.GrantID
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], audit)
	m.outbox = append(m.outbox, outbox)
	return cloneGrantDispatch(record), true, nil
}

func (m *Memory) ClaimGrantDispatch(_ context.Context, tenantID, runnerGroup, runnerID, deliveryToken string, now, leaseExpiresAt time.Time) (GrantDispatchRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.grantDispatch))
	for id := range m.grantDispatch {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := m.grantDispatch[ids[i]], m.grantDispatch[ids[j]]
		if left.CreatedAt.Equal(right.CreatedAt) {
			return ids[i] < ids[j]
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
	for _, id := range ids {
		record := m.grantDispatch[id]
		if record.Grant.TenantID != tenantID || record.Grant.RunnerGroup != runnerGroup || !record.Grant.ExpiresAt.After(now) || grantComplete(record.Status) {
			continue
		}
		if record.LeaseExpiresAt.After(now) {
			if record.RunnerID == runnerID {
				return cloneGrantDispatch(record), nil
			}
			continue
		}
		record.Status, record.RunnerID, record.DeliveryToken = GrantDispatchLeased, runnerID, deliveryToken
		record.LeaseExpiresAt, record.UpdatedAt, record.StateVersion = leaseExpiresAt, now, record.StateVersion+1
		m.grantDispatch[id] = cloneGrantDispatch(record)
		m.publishOutbox(record.OutboxID, now)
		return cloneGrantDispatch(record), nil
	}
	return GrantDispatchRecord{}, ErrNotFound
}

func (m *Memory) AcknowledgeGrantDispatch(_ context.Context, grantID, runnerID, token string, now, leaseExpiresAt time.Time) (GrantDispatchRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.grantDispatch[grantID]
	if !ok {
		return GrantDispatchRecord{}, ErrNotFound
	}
	if record.RunnerID != runnerID || record.DeliveryToken != token || grantComplete(record.Status) || !record.LeaseExpiresAt.After(now) {
		return GrantDispatchRecord{}, ErrConflict
	}
	record.Status, record.AcknowledgedAt, record.LeaseExpiresAt = GrantDispatchAcked, now, leaseExpiresAt
	record.UpdatedAt, record.StateVersion = now, record.StateVersion+1
	m.grantDispatch[grantID] = cloneGrantDispatch(record)
	return cloneGrantDispatch(record), nil
}

func (m *Memory) CompleteGrantDispatch(_ context.Context, grantID, runnerID, token string, result protocol.Result, now time.Time, audit domain.AuditEvent) (GrantDispatchRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.grantDispatch[grantID]
	if !ok {
		return GrantDispatchRecord{}, ErrNotFound
	}
	if grantComplete(record.Status) {
		if record.Result == result {
			return cloneGrantDispatch(record), nil
		}
		return GrantDispatchRecord{}, ErrConflict
	}
	if record.RunnerID != runnerID || record.DeliveryToken != token || result.GrantID != grantID || result.RunID != record.Grant.RunID || result.StepID != record.Grant.StepID {
		return GrantDispatchRecord{}, ErrConflict
	}
	record.Status = grantStatusForResult(result.Status)
	record.Result, record.CompletedAt, record.UpdatedAt = result, now, now
	record.StateVersion++
	m.grantDispatch[grantID] = cloneGrantDispatch(record)
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], audit)
	return cloneGrantDispatch(record), nil
}

func (m *Memory) GetGrantDispatch(_ context.Context, grantID string) (GrantDispatchRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.grantDispatch[grantID]
	if !ok {
		return GrantDispatchRecord{}, ErrNotFound
	}
	return cloneGrantDispatch(record), nil
}

func (m *Memory) publishOutbox(id string, now time.Time) {
	for index := range m.outbox {
		if m.outbox[index].ID == id && m.outbox[index].PublishedAt == nil {
			published := now
			m.outbox[index].PublishedAt = &published
			m.outbox[index].Attempts++
			return
		}
	}
}

func grantComplete(status GrantDispatchStatus) bool {
	return status == GrantDispatchSucceeded || status == GrantDispatchRejected || status == GrantDispatchUnknown
}

func grantStatusForResult(status protocol.ResultStatus) GrantDispatchStatus {
	switch status {
	case protocol.ResultSucceeded:
		return GrantDispatchSucceeded
	case protocol.ResultRejected:
		return GrantDispatchRejected
	default:
		return GrantDispatchUnknown
	}
}

func cloneGrantDispatch(record GrantDispatchRecord) GrantDispatchRecord {
	data, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	var clone GrantDispatchRecord
	if err := json.Unmarshal(data, &clone); err != nil {
		panic(err)
	}
	return clone
}

func (m *Memory) SavePlan(plan domain.ReleasePlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plans[plan.ID] = clonePlan(plan)
	return nil
}

func (m *Memory) GetPlan(id string) (domain.ReleasePlan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	plan, ok := m.plans[id]
	if !ok {
		return domain.ReleasePlan{}, ErrNotFound
	}
	return clonePlan(plan), nil
}

func (m *Memory) SetRunnerJournalError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journalError = err
}

func (m *Memory) CreateRun(run *domain.ReleaseRun) (*domain.ReleaseRun, bool, error) {
	return m.CreateRunAtomic(run, nil, nil)
}

func (m *Memory) CreateRunAtomic(run *domain.ReleaseRun, audit []domain.AuditEvent, outbox []domain.OutboxEvent) (*domain.ReleaseRun, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existingID, ok := m.runByRequestID[run.RequestID]; ok {
		return cloneRun(m.runs[existingID]), false, nil
	}
	if _, exists := m.runs[run.ID]; exists {
		return nil, false, fmt.Errorf("duplicate run id %q", run.ID)
	}
	stored := cloneRun(run)
	m.runs[run.ID] = stored
	m.projections[run.ID] = cloneRun(run)
	m.runByRequestID[run.RequestID] = run.ID
	m.appendAtomic(run.ID, audit, appendProjection(outbox, run))
	return cloneRun(stored), true, nil
}

func (m *Memory) GetRun(id string) (*domain.ReleaseRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	run, ok := m.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneRun(run), nil
}

func (m *Memory) ListRuns() ([]*domain.ReleaseRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	runs := make([]*domain.ReleaseRun, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, cloneRun(run))
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})
	return runs, nil
}

func (m *Memory) UpdateRun(run *domain.ReleaseRun, expectedVersion int64) error {
	return m.UpdateRunAtomic(run, expectedVersion, nil, nil)
}

func (m *Memory) UpdateRunAtomic(run *domain.ReleaseRun, expectedVersion int64, audit []domain.AuditEvent, outbox []domain.OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.runs[run.ID]
	if !ok {
		return ErrNotFound
	}
	if current.StateVersion != expectedVersion {
		return ErrConflict
	}
	updated := cloneRun(run)
	updated.StateVersion = expectedVersion + 1
	m.runs[run.ID] = updated
	m.projections[run.ID] = cloneRun(updated)
	run.StateVersion = updated.StateVersion
	m.appendAtomic(run.ID, audit, appendProjection(outbox, updated))
	return nil
}

func (m *Memory) AppendAudit(event domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit[event.CorrelationID] = append(m.audit[event.CorrelationID], event)
	return nil
}

func (m *Memory) AuditEvents(correlationID string) ([]domain.AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]domain.AuditEvent(nil), m.audit[correlationID]...), nil
}

func (m *Memory) ReserveExecution(record ExecutionRecord, audit domain.AuditEvent, outbox domain.OutboxEvent) (ExecutionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := executionKey(record.Adapter, record.IdempotencyKey)
	if existing, ok := m.executions[key]; ok {
		return cloneExecution(existing), false, nil
	}
	record.StateVersion = 1
	record.Status = ExecutionReserved
	m.executions[key] = cloneExecution(record)
	m.appendAtomic(record.RunID, []domain.AuditEvent{audit}, []domain.OutboxEvent{outbox})
	return cloneExecution(record), true, nil
}

func (m *Memory) CompleteExecution(record ExecutionRecord, expectedVersion int64, audit domain.AuditEvent, outbox domain.OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := executionKey(record.Adapter, record.IdempotencyKey)
	current, ok := m.executions[key]
	if !ok {
		return ErrNotFound
	}
	if current.StateVersion != expectedVersion {
		return ErrConflict
	}
	record.StateVersion = expectedVersion + 1
	m.executions[key] = cloneExecution(record)
	m.appendAtomic(record.RunID, []domain.AuditEvent{audit}, []domain.OutboxEvent{outbox})
	return nil
}

func (m *Memory) GetExecution(adapter, idempotencyKey string) (ExecutionRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.executions[executionKey(adapter, idempotencyKey)]
	if !ok {
		return ExecutionRecord{}, ErrNotFound
	}
	return cloneExecution(record), nil
}

func (m *Memory) PendingOutbox(limit int) ([]domain.OutboxEvent, error) {
	return m.pendingOutbox("", limit), nil
}

func (m *Memory) PendingOutboxByType(eventType string, limit int) ([]domain.OutboxEvent, error) {
	return m.pendingOutbox(eventType, limit), nil
}

func (m *Memory) pendingOutbox(eventType string, limit int) []domain.OutboxEvent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || limit > len(m.outbox) {
		limit = len(m.outbox)
	}
	result := make([]domain.OutboxEvent, 0, limit)
	for _, event := range m.outbox {
		if event.PublishedAt == nil && (eventType == "" || event.EventType == eventType) {
			result = append(result, event)
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

func (m *Memory) MarkOutboxPublished(id string, publishedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		if m.outbox[i].ID == id {
			m.outbox[i].PublishedAt = &publishedAt
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) RebuildProjection(runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	projection, ok := m.projections[runID]
	if !ok {
		return ErrNotFound
	}
	m.runs[runID] = cloneRun(projection)
	return nil
}

func (m *Memory) GetRunnerAction(_ context.Context, tenantID, runnerGroup, nonce string) (RunnerActionRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.journalError != nil {
		return RunnerActionRecord{}, m.journalError
	}
	record, ok := m.runnerActions[runnerNonceKey(tenantID, runnerGroup, nonce)]
	if !ok {
		return RunnerActionRecord{}, ErrNotFound
	}
	return record, nil
}

func (m *Memory) ReserveRunnerAction(_ context.Context, record RunnerActionRecord, audit domain.AuditEvent) (RunnerActionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journalError != nil {
		return RunnerActionRecord{}, false, m.journalError
	}
	nonceKey := runnerNonceKey(record.TenantID, record.RunnerGroup, record.Nonce)
	if existing, ok := m.runnerActions[nonceKey]; ok {
		return existing, false, nil
	}
	idempotencyKey := runnerIdempotencyKey(record.TenantID, record.RunnerGroup, record.IdempotencyKey)
	if existingNonce, ok := m.runnerByKey[idempotencyKey]; ok {
		return m.runnerActions[existingNonce], false, nil
	}
	record.Status, record.StateVersion = RunnerActionReserved, 1
	m.runnerActions[nonceKey] = record
	m.runnerByKey[idempotencyKey] = nonceKey
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], audit)
	return record, true, nil
}

func (m *Memory) CompleteRunnerAction(_ context.Context, record RunnerActionRecord, expected int64, audit domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journalError != nil {
		return m.journalError
	}
	key := runnerNonceKey(record.TenantID, record.RunnerGroup, record.Nonce)
	current, ok := m.runnerActions[key]
	if !ok {
		return ErrNotFound
	}
	if current.StateVersion != expected {
		return ErrConflict
	}
	if record.Status == RunnerActionDispatched && record.PlanRevision > 0 {
		for _, manifest := range m.manifests {
			if manifest.TenantID == record.TenantID && manifest.RunnerGroup == record.RunnerGroup && manifest.RunID == record.RunID && (manifest.Revision > record.PlanRevision || manifest.PlanHash != record.PlanHash) {
				return ErrConflict
			}
		}
	}
	record.StateVersion = expected + 1
	m.runnerActions[key] = record
	m.audit[audit.CorrelationID] = append(m.audit[audit.CorrelationID], audit)
	return nil
}

func (m *Memory) PendingRunnerActions(_ context.Context, tenantID, runnerGroup string, limit int) ([]RunnerActionRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.journalError != nil {
		return nil, m.journalError
	}
	result := []RunnerActionRecord{}
	for _, record := range m.runnerActions {
		if record.TenantID == tenantID && record.RunnerGroup == runnerGroup && (record.Status == RunnerActionReserved || record.Status == RunnerActionDispatched || record.Status == RunnerActionUnknown) {
			result = append(result, record)
			if limit > 0 && len(result) >= limit {
				break
			}
		}
	}
	return result, nil
}

func (m *Memory) appendAtomic(runID string, audit []domain.AuditEvent, outbox []domain.OutboxEvent) {
	for _, event := range audit {
		m.audit[event.CorrelationID] = append(m.audit[event.CorrelationID], event)
	}
	m.outbox = append(m.outbox, outbox...)
}

func appendProjection(events []domain.OutboxEvent, run *domain.ReleaseRun) []domain.OutboxEvent {
	payload, _ := json.Marshal(run)
	event := domain.OutboxEvent{
		ID: fmt.Sprintf("projection/%s/%d", run.ID, run.StateVersion), AggregateType: "release_run",
		AggregateID: run.ID, EventType: "release.run.project", CreatedAt: run.UpdatedAt,
		AvailableAt: run.UpdatedAt, Payload: map[string]any{"run": json.RawMessage(payload)},
	}
	return append(append([]domain.OutboxEvent(nil), events...), event)
}

func executionKey(adapter, idempotencyKey string) string { return adapter + "\x00" + idempotencyKey }
func runnerNonceKey(tenant, runnerGroup, nonce string) string {
	return tenant + "\x00" + runnerGroup + "\x00" + nonce
}
func runnerIdempotencyKey(tenant, runnerGroup, key string) string {
	return tenant + "\x00" + runnerGroup + "\x00" + key
}
func runnerFleetKey(tenant, runnerID string) string { return tenant + "\x00" + runnerID }

func cloneRun(run *domain.ReleaseRun) *domain.ReleaseRun {
	data, err := json.Marshal(run)
	if err != nil {
		panic(err)
	}
	var cloned domain.ReleaseRun
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return &cloned
}

func cloneExecution(record ExecutionRecord) ExecutionRecord {
	result := record
	if record.Payload != nil {
		result.Payload = make(map[string]any, len(record.Payload))
		for key, value := range record.Payload {
			result.Payload[key] = value
		}
	}
	return result
}

func clonePlan(plan domain.ReleasePlan) domain.ReleasePlan {
	data, err := json.Marshal(plan)
	if err != nil {
		panic(err)
	}
	var cloned domain.ReleasePlan
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return cloned
}
