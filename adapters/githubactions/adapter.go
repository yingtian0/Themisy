package githubactions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

const correlationRunNamePrefix = "themisy:"

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Adapter struct {
	config Config
	client HTTPDoer
	now    func() time.Time
}

func New(config Config, client HTTPDoer) (*Adapter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Adapter{config: config, client: client, now: time.Now}, nil
}

func (a *Adapter) Name() string    { return AdapterName }
func (a *Adapter) Version() string { return AdapterVersion }

func (a *Adapter) Authorize(target adapter.Target, _ string) error {
	if _, ok := a.config.Targets[TargetKey{Service: target.Service, Environment: target.Environment}]; !ok {
		return errors.New("target is not allow-listed")
	}
	return nil
}

func (a *Adapter) Deploy(ctx context.Context, request adapter.DeployRequest, credential credentials.Credential) (adapter.Deployment, error) {
	if err := adapter.ValidateDeployRequest(request); err != nil {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: "deploy.validate", Err: err}
	}
	target, ok := a.config.Targets[TargetKey{Service: request.Target.Service, Environment: request.Target.Environment}]
	if !ok {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: "deploy.target", Err: errors.New("target is not allow-listed")}
	}
	correlation := adapter.CorrelationID(request.IdempotencyKey)
	return a.dispatch(ctx, target, target.DeployWorkflow, map[string]string{
		"themisy_artifact_digest": request.ArtifactDigest,
		"themisy_environment":     request.Target.Environment,
		"themisy_idempotency_key": correlation,
		"themisy_service":         request.Target.Service,
	}, correlation, credential, "deploy")
}

func (a *Adapter) Rollback(ctx context.Context, request adapter.RollbackRequest, credential credentials.Credential) (adapter.Deployment, error) {
	if err := adapter.ValidateRollbackRequest(request); err != nil {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: "rollback.validate", Err: err}
	}
	target, ok := a.config.Targets[TargetKey{Service: request.Target.Service, Environment: request.Target.Environment}]
	if !ok {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: "rollback.target", Err: errors.New("target is not allow-listed")}
	}
	correlation := adapter.CorrelationID(request.IdempotencyKey)
	return a.dispatch(ctx, target, target.RollbackWorkflow, map[string]string{
		"themisy_environment":           request.Target.Environment,
		"themisy_idempotency_key":       correlation,
		"themisy_original_execution_id": request.OriginalDeployment.ExternalExecutionID,
		"themisy_service":               request.Target.Service,
	}, correlation, credential, "rollback")
}

func (a *Adapter) dispatch(ctx context.Context, target Target, workflow string, inputs map[string]string, correlation string, credential credentials.Credential, operation string) (adapter.Deployment, error) {
	if !credential.ValidAt(a.now()) || len(credential.Value("token")) == 0 {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: operation + ".credential", CorrelationID: correlation, Err: errors.New("valid GitHub credential required")}
	}
	body, err := json.Marshal(struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs"`
	}{Ref: target.Ref, Inputs: inputs})
	if err != nil {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: operation + ".encode", CorrelationID: correlation, Err: err}
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/dispatches", apiBaseURL, url.PathEscape(target.Owner), url.PathEscape(target.Repository), url.PathEscape(workflow))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: operation + ".request", CorrelationID: correlation, Err: err}
	}
	authorize(req, credential)
	started := a.now().UTC()
	response, err := a.client.Do(req)
	if err != nil {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorUnknown, Operation: operation + ".dispatch", CorrelationID: correlation, Err: errors.New("GitHub dispatch response unavailable")}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		class := adapter.ErrorTerminal
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			class = adapter.ErrorUnknown
		}
		return adapter.Deployment{}, &adapter.Error{Class: class, Operation: operation + ".dispatch", CorrelationID: correlation, Err: fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)}
	}
	var result struct {
		WorkflowRunID int64 `json:"workflow_run_id"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&result); err != nil || result.WorkflowRunID <= 0 {
		return adapter.Deployment{}, &adapter.Error{Class: adapter.ErrorUnknown, Operation: operation + ".dispatch", CorrelationID: correlation, Err: errors.New("GitHub returned no workflow run ID")}
	}
	return adapter.Deployment{ExternalExecutionID: strconv.FormatInt(result.WorkflowRunID, 10), CorrelationID: correlation, StartedAt: started}, nil
}

func authorize(request *http.Request, credential credentials.Credential) {
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+string(credential.Value("token")))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
}

func mapRunStatus(status, conclusion string) adapter.ReconcileStatus {
	if status != "completed" {
		return adapter.ReconcilePending
	}
	if conclusion == "success" {
		return adapter.ReconcileSucceeded
	}
	return adapter.ReconcileFailed
}

func validRunTitle(title, correlation string) bool {
	return strings.TrimSpace(title) == correlationRunNamePrefix+correlation
}
