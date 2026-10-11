//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"themisy/internal/api"
	"themisy/internal/application"
	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/planner"
	"themisy/internal/policy"
	"themisy/internal/profile"
	postgresstore "themisy/internal/store/postgres"
	workflowcore "themisy/internal/workflow"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

func TestTemporalAPIWorkerRestartApprovalCancelAndReplay(t *testing.T) {
	requireIntegration(t)
	databaseURL := integrationDatabaseURL()
	if err := postgresstore.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	persistent, err := postgresstore.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer persistent.Close()
	address := os.Getenv("THEMISY_TEMPORAL_ADDRESS")
	if address == "" {
		address = "localhost:7233"
	}
	probeWorkflowID := fmt.Sprintf("namespace-readiness-%d", time.Now().UnixNano())
	var temporalClient client.Client
	eventually(t, 60*time.Second, func() error {
		attemptCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		candidate, err := client.DialContext(attemptCtx, client.Options{HostPort: address, Namespace: "default"})
		if err != nil {
			return err
		}
		_, healthErr := candidate.CheckHealth(attemptCtx, &client.CheckHealthRequest{})
		if healthErr != nil {
			candidate.Close()
			return healthErr
		}
		// The frontend can be healthy before auto-setup registers the namespace.
		namespace, err := candidate.WorkflowService().DescribeNamespace(attemptCtx, &workflowservice.DescribeNamespaceRequest{Namespace: "default"})
		if err != nil {
			candidate.Close()
			return err
		}
		if namespace.GetNamespaceInfo().GetState() != enumspb.NAMESPACE_STATE_REGISTERED {
			candidate.Close()
			return fmt.Errorf("default namespace is not registered: %s", namespace.GetNamespaceInfo().GetState())
		}
		// Registration can precede namespace-cache propagation to workflow APIs.
		// A missing workflow confirms that the namespace itself was resolved;
		// NamespaceNotFound is a distinct error and must remain retryable here.
		_, err = candidate.WorkflowService().DescribeWorkflowExecution(attemptCtx, &workflowservice.DescribeWorkflowExecutionRequest{
			Namespace: "default", Execution: &commonpb.WorkflowExecution{WorkflowId: probeWorkflowID},
		})
		var workflowNotFound *serviceerror.NotFound
		if err != nil && !errors.As(err, &workflowNotFound) {
			candidate.Close()
			return err
		}
		temporalClient = candidate
		return nil
	})
	defer temporalClient.Close()
	queue := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	activities := workflowcore.NewActivities(persistent, policy.New(), executor.NewMock(nil))
	firstWorker := workflowcore.NewWorker(temporalClient, queue, activities)
	if err := firstWorker.Start(); err != nil {
		t.Fatal(err)
	}
	contracts, err := contract.LoadDir("../../examples/contracts")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.LoadDir("../../examples/profiles")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the real verification path in this integration test without making
	// CI wait for the production observation windows declared by the examples.
	for index := range profiles {
		profiles[index].Spec.Verification.ObservationWindow = "1ms"
		profiles[index].ContentHash = ""
	}
	p, err := planner.NewFromContracts(contracts, profiles, planner.Options{})
	if err != nil {
		t.Fatal(err)
	}
	releases := application.NewReleases(p, persistent, workflowcore.NewController(temporalClient, queue))
	handler := api.New(releases, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	requestID := fmt.Sprintf("temporal-api-%d", time.Now().UnixNano())
	body := []byte(fmt.Sprintf(`{"request_id":%q,"release_version":"release-1","environment":"staging","requested_by":"user-1","delegated_agent":{"id":"agent-1","scopes":["release:deploy"]},"changes":[{"service":"identity-api","desired_version":"sha-1","ci_success":true,"dependencies_healthy":true}]}`, requestID))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/release-runs", bytes.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var started domain.ReleaseRun
	if err := json.NewDecoder(response.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, func() error {
		run, err := releases.Get(started.ID)
		if err != nil {
			return err
		}
		if run.Status != domain.RunSucceeded {
			return fmt.Errorf("status=%s", run.Status)
		}
		return nil
	})
	firstWorker.Stop()
	destructive := domain.ReleaseRequest{RequestID: fmt.Sprintf("restart-approval-%d", time.Now().UnixNano()), ReleaseVersion: "release-1", Environment: domain.EnvironmentProduction, RequestedBy: "requester", Agent: domain.AgentIdentity{ID: "agent", Scopes: []string{"release:deploy", "release:production"}}, Changes: []domain.Change{{Service: "identity-api", DesiredVersion: "sha-2", CISuccess: true, DependenciesHealthy: true, DestructiveMigration: true}}}
	secondWorker := workflowcore.NewWorker(temporalClient, queue, activities)
	if err := secondWorker.Start(); err != nil {
		t.Fatal(err)
	}
	waiting, _, err := releases.Start(context.Background(), destructive)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, func() error {
		run, err := releases.Get(waiting.ID)
		if err != nil {
			return err
		}
		if run.Status != domain.RunWaitingApproval {
			return fmt.Errorf("status=%s", run.Status)
		}
		waiting = run
		return nil
	})
	secondWorker.Stop()
	approvalID := waiting.Steps[0].Approval.ID
	if _, err := releases.DecideApproval(context.Background(), waiting.ID, approvalID, "sre-user", []string{"service-owner", "sre"}, true); err != nil {
		t.Fatal(err)
	}
	thirdWorker := workflowcore.NewWorker(temporalClient, queue, activities)
	if err := thirdWorker.Start(); err != nil {
		t.Fatal(err)
	}
	defer thirdWorker.Stop()
	eventually(t, 20*time.Second, func() error {
		run, err := releases.Get(waiting.ID)
		if err != nil {
			return err
		}
		if run.Status != domain.RunSucceeded {
			return fmt.Errorf("status=%s", run.Status)
		}
		return nil
	})
	cancelRequest := destructive
	cancelRequest.RequestID = fmt.Sprintf("cancel-%d", time.Now().UnixNano())
	cancelRequest.Changes[0].DesiredVersion = "sha-3"
	cancelled, _, err := releases.Start(context.Background(), cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, func() error {
		run, err := releases.Get(cancelled.ID)
		if err != nil {
			return err
		}
		if run.Status != domain.RunWaitingApproval {
			return fmt.Errorf("status=%s", run.Status)
		}
		return nil
	})
	if _, err := releases.Cancel(cancelled.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 20*time.Second, func() error {
		run, err := releases.Get(cancelled.ID)
		if err != nil {
			return err
		}
		if run.Status != domain.RunCancelled {
			return fmt.Errorf("status=%s", run.Status)
		}
		return nil
	})
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(workflowcore.ReleaseWorkflow)
	if err := replayer.ReplayWorkflowExecution(context.Background(), temporalClient.WorkflowService(), nil, "default", temporalworkflow.Execution{ID: started.WorkflowID}); err != nil {
		t.Fatal(err)
	}
}
