package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"themisy/internal/api"
	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/store"
	workflowcore "themisy/internal/workflow"
)

func TestCLIPlanUsesSameApplicationPlanHash(t *testing.T) {
	p, err := planner.New([]domain.Service{{Name: "identity", OwnerTeam: "identity", RiskTier: "high"}})
	if err != nil {
		t.Fatal(err)
	}
	service := application.NewReleases(p, store.NewMemory(), noopController{})
	server := httptest.NewServer(api.New(service, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	defer server.Close()
	input := []byte(`{"api_version":"execution.themisy.io/v1alpha1","kind":"ReleaseIntent","request_id":"cli-plan","release_version":"release-1","tenant_id":"tenant-a","environment":"staging","requested_by":"user","delegated_agent":{"id":"agent","scopes":["release:deploy"]},"changes":[{"service":"identity","desired_version":"sha-1","ci_success":true,"dependencies_healthy":true}]}`)
	var intent domain.ReleaseIntent
	if err := json.Unmarshal(input, &intent); err != nil {
		t.Fatal(err)
	}
	want, err := service.PlanIntentForTenant("tenant-a", intent)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(filename, input, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"-server", server.URL, "-tenant", "tenant-a", "plan", filename}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var got domain.ReleasePlan
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Hash != want.Hash {
		t.Fatalf("CLI hash=%s application hash=%s", got.Hash, want.Hash)
	}
}

func TestCLIRequiresActorForMutation(t *testing.T) {
	err := run(context.Background(), []string{"pause", "run-1"}, io.Discard, io.Discard)
	if err == nil || err.Error() != "-actor or THEMISY_ACTOR_ID is required for this command" {
		t.Fatalf("error=%v", err)
	}
}

func TestCLIRootHelpShowsOfflineEntryAndReturnsSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, &stdout, &stderr); err != nil {
		t.Fatalf("help returned error: %v", err)
	}
	for _, want := range []string{"Usage: themisyctl", "offline-plan", "examples/offline/release.json", "examples/offline/rejected.json", "docs/guides/offline-quickstart.md"} {
		if !bytes.Contains(stderr.Bytes(), []byte(want)) {
			t.Errorf("root help missing %q: %s", want, stderr.String())
		}
	}
}

type noopController struct{}

func (noopController) StartRelease(context.Context, workflowcore.ReleaseInput) (workflowcore.Execution, error) {
	return workflowcore.Execution{}, nil
}
func (noopController) SignalApproval(context.Context, string, workflowcore.ApprovalSignal) error {
	return nil
}
func (noopController) Pause(context.Context, string, workflowcore.ControlSignal) error  { return nil }
func (noopController) Resume(context.Context, string, workflowcore.ControlSignal) error { return nil }
func (noopController) Cancel(context.Context, string, workflowcore.ControlSignal) error { return nil }
