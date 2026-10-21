package githubactions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

const ciEvidenceSchema = "themisy.ci-evidence/v1alpha1"
const ciResponseLimit = 2 << 20

var gitCommitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

type CITargetKey struct{ TenantID, Service, Environment string }

// CITarget is an operator allowlist. The request cannot choose a repository,
// workflow, or API URL.
type CITarget struct {
	TenantID   string
	Owner      string
	Repository string
	Workflow   string
	MaximumAge time.Duration
}

type CIConfig struct{ Targets map[CITargetKey]CITarget }

func (c CIConfig) Validate() error {
	if len(c.Targets) == 0 {
		return errors.New("at least one CI target is required")
	}
	for key, target := range c.Targets {
		if strings.TrimSpace(key.TenantID) == "" || strings.TrimSpace(key.Service) == "" || strings.TrimSpace(key.Environment) == "" ||
			!repositoryPart.MatchString(target.Owner) || !repositoryPart.MatchString(target.Repository) ||
			(!workflowFile.MatchString(target.Workflow) && !decimalWorkflowID(target.Workflow)) ||
			target.MaximumAge < time.Minute || target.MaximumAge > 30*24*time.Hour {
			return fmt.Errorf("invalid CI target for %s/%s", key.Service, key.Environment)
		}
	}
	return nil
}

type CIRequest struct {
	TenantID  string
	Target    adapter.Target
	CommitSHA string
}

type CIArtifact struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// CIEvidence records the exact source selector and the GitHub run/artifact
// identities. Artifact digests are GitHub artifact archive digests, not image
// or build provenance digests.
type CIEvidence struct {
	SchemaVersion   string                     `json:"schema_version"`
	Status          adapter.VerificationStatus `json:"status"`
	ReasonCode      string                     `json:"reason_code"`
	Source          string                     `json:"source"`
	Repository      string                     `json:"repository"`
	CommitSHA       string                     `json:"commit_sha"`
	Workflow        string                     `json:"workflow"`
	QueryHash       string                     `json:"query_hash"`
	SourceUpdatedAt time.Time                  `json:"source_updated_at,omitempty"`
	Freshness       time.Duration              `json:"freshness,omitempty"`
	MaximumAge      time.Duration              `json:"maximum_age"`
	RunID           int64                      `json:"run_id,omitempty"`
	RunAttempt      int                        `json:"run_attempt,omitempty"`
	Conclusion      string                     `json:"conclusion,omitempty"`
	Artifacts       []CIArtifact               `json:"artifacts,omitempty"`
	ObservedAt      time.Time                  `json:"observed_at"`
	EvidenceHash    string                     `json:"evidence_hash"`
}

type CIAdapter struct {
	config CIConfig
	client HTTPDoer
	Now    func() time.Time
}

func NewCIAdapter(config CIConfig, client HTTPDoer) (*CIAdapter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &CIAdapter{config: config, client: client, Now: time.Now}, nil
}

// ReadCI checks only the exact configured repository/workflow and requested
// commit. It performs GET requests and never downloads logs or artifact bodies.
func (a *CIAdapter) ReadCI(ctx context.Context, request CIRequest, credential credentials.Credential) (CIEvidence, error) {
	if a == nil || a.client == nil || request.TenantID == "" || request.Target.Service == "" || request.Target.Environment == "" || !gitCommitSHA.MatchString(request.CommitSHA) {
		return CIEvidence{}, errors.New("invalid CI evidence request")
	}
	target, ok := a.config.Targets[CITargetKey{TenantID: request.TenantID, Service: request.Target.Service, Environment: request.Target.Environment}]
	if !ok {
		return CIEvidence{}, errors.New("CI target is not allow-listed")
	}
	if !credential.ValidAt(a.now()) || len(credential.Value("token")) == 0 {
		return CIEvidence{}, errors.New("valid GitHub read credential required")
	}
	base := apiBaseURL + "/repos/" + url.PathEscape(target.Owner) + "/" + url.PathEscape(target.Repository)
	workflow := url.PathEscape(target.Workflow)
	runsURL := base + "/actions/workflows/" + workflow + "/runs?head_sha=" + url.QueryEscape(strings.ToLower(request.CommitSHA)) + "&per_page=100"
	queryHash := ciQueryHash(request, target)
	runData, status, err := a.getJSON(ctx, runsURL, credential)
	if err != nil {
		return a.ciFailure(request, target, queryHash, status)
	}
	var runs struct {
		TotalCount int `json:"total_count"`
		Runs       []struct {
			ID         int64     `json:"id"`
			WorkflowID int64     `json:"workflow_id"`
			HeadSHA    string    `json:"head_sha"`
			Path       string    `json:"path"`
			Status     string    `json:"status"`
			Conclusion string    `json:"conclusion"`
			RunAttempt int       `json:"run_attempt"`
			UpdatedAt  time.Time `json:"updated_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(runData, &runs); err != nil {
		return a.evidence(request, target, queryHash, adapter.VerificationInconclusive, "CI_RUN_RESPONSE_INVALID", "", 0, 0, nil), nil
	}
	if runs.TotalCount < 0 || runs.TotalCount != len(runs.Runs) {
		return a.evidence(request, target, queryHash, adapter.VerificationInconclusive, "CI_RUN_LIST_PARTIAL", "", 0, 0, nil), nil
	}
	if runs.TotalCount == 0 {
		return a.evidence(request, target, queryHash, adapter.VerificationMissing, "CI_RUN_MISSING", "", 0, 0, nil), nil
	}
	for _, run := range runs.Runs {
		workflowMatches := workflowPathMatches(run.Path, target.Workflow)
		if decimalWorkflowID(target.Workflow) {
			workflowID, _ := strconv.ParseInt(target.Workflow, 10, 64)
			workflowMatches = run.WorkflowID == workflowID
		}
		if !strings.EqualFold(run.HeadSHA, request.CommitSHA) || !workflowMatches || run.ID <= 0 || run.RunAttempt < 1 {
			return a.evidence(request, target, queryHash, adapter.VerificationInconclusive, "CI_RUN_IDENTITY_MISMATCH", "", run.ID, run.RunAttempt, nil), nil
		}
	}
	sort.Slice(runs.Runs, func(i, j int) bool {
		a, b := runs.Runs[i], runs.Runs[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if a.ID != b.ID {
			return a.ID > b.ID
		}
		return a.RunAttempt > b.RunAttempt
	})
	selected := runs.Runs[0]
	if selected.Status != "completed" {
		return a.evidenceForRun(request, target, queryHash, adapter.VerificationInconclusive, "CI_RUN_INCOMPLETE", selected.Conclusion, selected.ID, selected.RunAttempt, selected.UpdatedAt, nil), nil
	}
	freshness := a.now().Sub(selected.UpdatedAt)
	if selected.UpdatedAt.IsZero() || freshness < 0 || freshness > target.MaximumAge {
		return a.evidenceForRun(request, target, queryHash, adapter.VerificationInconclusive, "CI_RUN_STALE", selected.Conclusion, selected.ID, selected.RunAttempt, selected.UpdatedAt, nil), nil
	}
	if selected.Conclusion != "success" {
		return a.evidenceForRun(request, target, queryHash, adapter.VerificationFail, "CI_RUN_FAILED", selected.Conclusion, selected.ID, selected.RunAttempt, selected.UpdatedAt, nil), nil
	}
	artifactsURL := base + "/actions/runs/" + strconv.FormatInt(selected.ID, 10) + "/artifacts?per_page=100"
	artifactData, artifactStatus, err := a.getJSON(ctx, artifactsURL, credential)
	if err != nil {
		return a.ciFailure(request, target, queryHash, artifactStatus)
	}
	var artifactResponse struct {
		TotalCount int `json:"total_count"`
		Artifacts  []struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Digest      string `json:"digest"`
			Expired     bool   `json:"expired"`
			WorkflowRun struct {
				ID      int64  `json:"id"`
				HeadSHA string `json:"head_sha"`
			} `json:"workflow_run"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(artifactData, &artifactResponse); err != nil {
		return a.evidenceForRun(request, target, queryHash, adapter.VerificationInconclusive, "ARTIFACT_RESPONSE_INVALID", "success", selected.ID, selected.RunAttempt, selected.UpdatedAt, nil), nil
	}
	if artifactResponse.TotalCount < 0 || artifactResponse.TotalCount != len(artifactResponse.Artifacts) {
		return a.evidenceForRun(request, target, queryHash, adapter.VerificationInconclusive, "ARTIFACT_LIST_PARTIAL", "success", selected.ID, selected.RunAttempt, selected.UpdatedAt, nil), nil
	}
	artifacts := make([]CIArtifact, 0, len(artifactResponse.Artifacts))
	for _, artifact := range artifactResponse.Artifacts {
		if artifact.Expired {
			continue
		}
		if artifact.ID <= 0 || artifact.WorkflowRun.ID != selected.ID || !strings.EqualFold(artifact.WorkflowRun.HeadSHA, request.CommitSHA) || artifact.Name == "" || !validSHA256Digest(artifact.Digest) {
			return a.evidenceForRun(request, target, queryHash, adapter.VerificationInconclusive, "ARTIFACT_IDENTITY_MISMATCH", "success", selected.ID, selected.RunAttempt, selected.UpdatedAt, nil), nil
		}
		artifacts = append(artifacts, CIArtifact{ID: artifact.ID, Name: artifact.Name, Digest: strings.ToLower(artifact.Digest)})
	}
	if len(artifacts) == 0 {
		return a.evidenceForRun(request, target, queryHash, adapter.VerificationMissing, "CI_ARTIFACT_MISSING", "success", selected.ID, selected.RunAttempt, selected.UpdatedAt, nil), nil
	}
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].Name == artifacts[j].Name {
			return artifacts[i].Digest < artifacts[j].Digest
		}
		return artifacts[i].Name < artifacts[j].Name
	})
	return a.evidenceForRun(request, target, queryHash, adapter.VerificationPass, "CI_RUN_SUCCEEDED", "success", selected.ID, selected.RunAttempt, selected.UpdatedAt, artifacts), nil
}

func workflowPathMatches(path, configured string) bool {
	workflowPath := strings.SplitN(path, "@", 2)[0]
	return strings.HasSuffix(workflowPath, "/"+configured) || workflowPath == configured
}

func validSHA256Digest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil
}

func decimalWorkflowID(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return err == nil
}

func (a *CIAdapter) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func ciQueryHash(request CIRequest, target CITarget) string {
	payload, _ := json.Marshal(struct{ Tenant, Service, Environment, Owner, Repository, Workflow, Commit string }{
		request.TenantID, request.Target.Service, request.Target.Environment, target.Owner, target.Repository, target.Workflow, strings.ToLower(request.CommitSHA),
	})
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (a *CIAdapter) getJSON(ctx context.Context, endpoint string, credential credentials.Credential) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+string(credential.Value("token")))
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	response, err := a.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.Request != nil && response.Request.URL.String() != endpoint {
		return nil, response.StatusCode, errors.New("GitHub API redirect refused")
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, response.StatusCode, fmt.Errorf("GitHub API returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, ciResponseLimit+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(data) > ciResponseLimit {
		return nil, response.StatusCode, errors.New("GitHub API response exceeds size limit")
	}
	return data, response.StatusCode, nil
}

func (a *CIAdapter) ciFailure(request CIRequest, target CITarget, queryHash string, status int) (CIEvidence, error) {
	reason := "SOURCE_UNAVAILABLE"
	if status == http.StatusNotFound {
		reason = "WORKFLOW_OR_REPOSITORY_MISSING"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		reason = "SOURCE_UNAUTHORIZED"
	}
	if status == http.StatusTooManyRequests {
		reason = "SOURCE_RATE_LIMITED"
	}
	return a.evidence(request, target, queryHash, adapter.VerificationInconclusive, reason, "", 0, 0, nil), nil
}

func (a *CIAdapter) evidence(request CIRequest, target CITarget, queryHash string, status adapter.VerificationStatus, reason, conclusion string, runID int64, attempt int, artifacts []CIArtifact) CIEvidence {
	evidence := CIEvidence{
		SchemaVersion: ciEvidenceSchema, Status: status, ReasonCode: reason, Source: AdapterName,
		Repository: target.Owner + "/" + target.Repository, CommitSHA: strings.ToLower(request.CommitSHA),
		Workflow: target.Workflow, QueryHash: queryHash, RunID: runID, RunAttempt: attempt,
		Conclusion: conclusion, Artifacts: artifacts, ObservedAt: a.now(), MaximumAge: target.MaximumAge,
	}
	evidence.EvidenceHash = hashCIEvidence(evidence)
	return evidence
}

func (a *CIAdapter) evidenceForRun(request CIRequest, target CITarget, queryHash string, status adapter.VerificationStatus, reason, conclusion string, runID int64, attempt int, updatedAt time.Time, artifacts []CIArtifact) CIEvidence {
	evidence := a.evidence(request, target, queryHash, status, reason, conclusion, runID, attempt, artifacts)
	evidence.SourceUpdatedAt = updatedAt.UTC()
	if !updatedAt.IsZero() {
		evidence.Freshness = evidence.ObservedAt.Sub(updatedAt)
	}
	evidence.MaximumAge = target.MaximumAge
	evidence.EvidenceHash = hashCIEvidence(evidence)
	return evidence
}

func hashCIEvidence(evidence CIEvidence) string {
	evidence.EvidenceHash = ""
	evidence.ObservedAt = evidence.ObservedAt.UTC()
	evidence.CommitSHA = strings.ToLower(evidence.CommitSHA)
	evidence.Artifacts = append([]CIArtifact(nil), evidence.Artifacts...)
	sort.Slice(evidence.Artifacts, func(i, j int) bool {
		if evidence.Artifacts[i].Name == evidence.Artifacts[j].Name {
			return evidence.Artifacts[i].Digest < evidence.Artifacts[j].Digest
		}
		return evidence.Artifacts[i].Name < evidence.Artifacts[j].Name
	})
	payload, _ := json.Marshal(evidence)
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
