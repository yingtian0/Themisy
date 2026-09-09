package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"themisy/pkg/adapter"
)

type DeployRequest struct {
	RunID          string
	RequestedBy    string
	AgentID        string
	Service        string
	Environment    string
	DesiredVersion string
	IdempotencyKey string
}

type Deployment struct {
	ExternalID     string
	StartedAt      time.Time
	FinishedAt     time.Time
	RunID          string
	Service        string
	Environment    string
	DesiredVersion string
	IdempotencyKey string
}

type VerificationResult struct {
	Status        adapter.VerificationStatus
	Healthy       bool
	Reason        string
	ObservedValue float64
	Threshold     float64
	Evidence      adapter.Evidence
}

func (r VerificationResult) Outcome() adapter.VerificationStatus {
	if r.Status.Valid() {
		return r.Status
	}
	if r.Healthy {
		return adapter.VerificationPass
	}
	return adapter.VerificationFail
}

type ReleaseExecutor interface {
	Deploy(context.Context, DeployRequest) (Deployment, error)
	Verify(context.Context, Deployment) (VerificationResult, error)
	Rollback(context.Context, Deployment) (Deployment, error)
}

type MockBehavior struct {
	DeployError          bool
	VerifyHealthy        bool
	VerifyError          bool
	RollbackError        bool
	VerifyStatus         adapter.VerificationStatus
	RollbackVerifyStatus adapter.VerificationStatus
	RollbackVerifyError  bool
}

type Mock struct {
	mu           sync.Mutex
	deployments  map[string]Deployment
	serviceByID  map[string]string
	rollbackByID map[string]bool
	behavior     map[string]MockBehavior
	deployCalls  map[string]int
}

func NewMock(behavior map[string]MockBehavior) *Mock {
	return &Mock{
		deployments: make(map[string]Deployment), serviceByID: make(map[string]string), rollbackByID: make(map[string]bool),
		behavior: behavior, deployCalls: make(map[string]int),
	}
}

func (m *Mock) Deploy(_ context.Context, request DeployRequest) (Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.deployments[request.IdempotencyKey]; ok {
		return existing, nil
	}
	m.deployCalls[request.IdempotencyKey]++
	if m.behavior[request.Service].DeployError {
		return Deployment{}, errors.New("simulated deploy failure")
	}
	now := time.Now().UTC()
	deployment := Deployment{
		ExternalID: fmt.Sprintf("mock-%s-%d", request.Service, len(m.deployments)+1),
		StartedAt:  now, FinishedAt: now,
	}
	m.deployments[request.IdempotencyKey] = deployment
	m.serviceByID[deployment.ExternalID] = request.Service
	return deployment, nil
}

func (m *Mock) Verify(_ context.Context, deployment Deployment) (VerificationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	behavior, configured := m.behavior[m.serviceByID[deployment.ExternalID]]
	if m.rollbackByID[deployment.ExternalID] {
		if behavior.RollbackVerifyError {
			return VerificationResult{}, errors.New("simulated rollback metrics failure")
		}
		status := behavior.RollbackVerifyStatus
		if !status.Valid() {
			status = adapter.VerificationPass
		}
		return mockVerification(deployment, status, "simulated rollback verification"), nil
	}
	if behavior.VerifyError {
		return VerificationResult{}, errors.New("simulated metrics failure")
	}
	status := behavior.VerifyStatus
	if !status.Valid() && configured && !behavior.VerifyHealthy {
		status = adapter.VerificationFail
	}
	if !status.Valid() {
		status = adapter.VerificationPass
	}
	return mockVerification(deployment, status, "mock health check"), nil
}

func (m *Mock) Rollback(_ context.Context, deployment Deployment) (Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.behavior[m.serviceByID[deployment.ExternalID]].RollbackError {
		return Deployment{}, errors.New("simulated rollback failure")
	}
	now := time.Now().UTC()
	rolledBack := Deployment{ExternalID: "rollback-" + deployment.ExternalID, StartedAt: now, FinishedAt: now}
	m.serviceByID[rolledBack.ExternalID] = m.serviceByID[deployment.ExternalID]
	m.rollbackByID[rolledBack.ExternalID] = true
	return rolledBack, nil
}

func (m *Mock) DeployCalls(idempotencyKey string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deployCalls[idempotencyKey]
}

func mockVerification(deployment Deployment, status adapter.VerificationStatus, reason string) VerificationResult {
	now := time.Now().UTC()
	value := 0.1
	if status == adapter.VerificationFail {
		value = 8.3
	}
	evidence := adapter.Evidence{Status: status, ReasonCode: string(status), Source: "mock", QueryHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Window: adapter.ObservationWindow{From: deployment.StartedAt, To: now}, ObservedAt: now, ObservedValue: value, Threshold: 1, AdapterVersion: "mock/v1"}
	if !evidence.Window.To.After(evidence.Window.From) {
		evidence.Window.From = now.Add(-time.Minute)
	}
	evidence.EvidenceHash, _ = adapter.EvidenceHash(evidence)
	return VerificationResult{Status: status, Healthy: status == adapter.VerificationPass, Reason: reason, ObservedValue: value, Threshold: 1, Evidence: evidence}
}
