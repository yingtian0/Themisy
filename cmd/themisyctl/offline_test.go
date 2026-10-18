package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/profile"
)

type rejectOfflineTransport struct{ calls int }

func (r *rejectOfflineTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("offline command attempted HTTP")
}

func TestOfflinePlanMatchesPlannerWithoutNetwork(t *testing.T) {
	transport := &rejectOfflineTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old })
	const at = "2026-10-18T00:00:00Z"
	root := filepath.Join("..", "..")
	contractsDir := filepath.Join(root, "examples", "contracts")
	profilesDir := filepath.Join(root, "examples", "profiles")
	intentPath := filepath.Join(root, "examples", "offline", "release.json")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-server", "http://invalid.invalid", "offline-plan", "-contracts", contractsDir, "-profiles", profilesDir, "-at", at, intentPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Mode   string             `json:"mode"`
		Notice string             `json:"notice"`
		Plan   domain.ReleasePlan `json:"plan"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	contracts, err := contract.LoadDir(contractsDir)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.LoadDir(profilesDir)
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, at)
	p, err := planner.NewFromContracts(contracts, profiles, planner.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	var intent domain.ReleaseIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		t.Fatal(err)
	}
	want, err := p.PlanIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Plan, want) {
		t.Fatalf("offline plan differs from core planner: got=%+v want=%+v", got.Plan, want)
	}
	if got.Mode != "offline" || got.Notice != offlineNotice || !strings.Contains(stderr.String(), offlineNotice) {
		t.Fatal("missing offline boundary notice")
	}
	if transport.calls != 0 {
		t.Fatalf("HTTP calls=%d", transport.calls)
	}
}

func TestOfflinePlanRejectsInvalidInputs(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, tc := range []struct{ name, content, reason string }{
		{"unknown-service", `{"api_version":"execution.themisy.io/v1alpha1","kind":"ReleaseIntent","environment":"staging","changes":[{"service":"missing-api","desired_version":"v1"}]}`, `unknown service "missing-api"`},
		{"schema", `{"api_version":"bad","kind":"ReleaseIntent"}`, "UNSUPPORTED_SCHEMA_VERSION"},
		{"unknown-field", `{"command":"shell"}`, "unknown field"},
		{"trailing", `{} {}`, "exactly one JSON document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release.json")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			err := run(context.Background(), []string{"offline-plan", "-contracts", filepath.Join(root, "examples/contracts"), "-profiles", filepath.Join(root, "examples/profiles"), path}, &stdout, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.reason) || stdout.Len() != 0 {
				t.Fatalf("error=%v stdout=%s", err, stdout.String())
			}
		})
	}
}

func TestOfflinePlanRejectsContractGraph(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile("../../examples/contracts/identity-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("dependencies: []"), []byte("dependencies:\n  - service: missing-api\n    type: rollout-order"), 1)
	if err := os.WriteFile(filepath.Join(dir, "identity.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{"offline-plan", "-contracts", dir, "-profiles", "../../examples/profiles", "../../examples/offline/release.json"}, io.Discard, io.Discard)
	if code, ok := domain.ReasonOf(err); !ok || code != domain.ReasonUnknownDependency {
		t.Fatalf("error=%v", err)
	}
}

func TestOfflinePlanRejectsIsolatedRolloutOrderCycle(t *testing.T) {
	root := filepath.Join("..", "..")
	var stdout bytes.Buffer
	err := run(context.Background(), []string{
		"offline-plan",
		"-contracts", filepath.Join(root, "examples/offline/cycle/contracts"),
		"-profiles", filepath.Join(root, "examples/profiles"),
		filepath.Join(root, "examples/offline/cycle/release.json"),
	}, &stdout, io.Discard)
	if code, ok := domain.ReasonOf(err); !ok || code != domain.ReasonDependencyCycle {
		t.Fatalf("error=%v, want %s", err, domain.ReasonDependencyCycle)
	}
	if stdout.Len() != 0 {
		t.Fatalf("cycle rejection emitted a plan: %s", stdout.String())
	}
}
