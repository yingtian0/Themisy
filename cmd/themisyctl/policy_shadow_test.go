package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/policy"
	policyfiles "themisy/policies"
)

func shadowCLIInputs(t *testing.T) ([]string, string) {
	t.Helper()
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	signer, public, err := grant.NewDevelopmentSigner("test-policy-key")
	if err != nil {
		t.Fatal(err)
	}
	source, err := policyfiles.FS.ReadFile("baseline/main.rego")
	if err != nil {
		t.Fatal(err)
	}
	current, err := policy.SignBundle(context.Background(), signer, policy.Bundle{Version: "v1", Issuer: "test-issuer", Compatibility: []string{policy.InputVersionV1Alpha1}, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Modules: []policy.Module{{Name: "main.rego", Layer: policy.LayerPlatform, Source: string(source)}}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := current
	candidate.Version = "v2"
	candidate.Modules = append([]policy.Module(nil), current.Modules...)
	candidate.Modules = append(candidate.Modules, policy.Module{Name: "service.rego", Layer: policy.LayerService, Source: "package themisy.authorization\nimport rego.v1\ndeny contains \"SHADOW_FREEZE\" if { input.service == \"identity\" }"})
	candidate, err = policy.SignBundle(context.Background(), signer, candidate)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	save := func(name string, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save("current.json", current)
	save("candidate.json", candidate)
	save("cases.json", []policy.ShadowCase{{ID: "identity", Input: policy.Input{UserID: "synthetic-user", AgentID: "synthetic-agent", Service: "identity", Environment: "staging"}}})
	if err := os.WriteFile(filepath.Join(directory, "public.key"), []byte(base64.RawURLEncoding.EncodeToString(public)), 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"policy-shadow", "-current", filepath.Join(directory, "current.json"), "-candidate", filepath.Join(directory, "candidate.json"), "-cases", filepath.Join(directory, "cases.json"), "-issuer", "test-issuer", "-key-id", "test-policy-key", "-public-key", filepath.Join(directory, "public.key"), "-at", now.Format(time.RFC3339)}, directory
}

func TestPolicyShadowCLIIsOfflineAndDoesNotApply(t *testing.T) {
	args, _ := shadowCLIInputs(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	args = append([]string{"-server", server.URL, "-actor", "synthetic-actor"}, args...)
	var out, errOut bytes.Buffer
	if err := run(context.Background(), args, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var report policy.ShadowReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Mode != policy.ShadowReportMode || report.ChangedCases != 1 || len(report.Cases) != 1 || report.Cases[0].Current.Decision != domain.DecisionAllow || report.Cases[0].Candidate.Decision != domain.DecisionDeny {
		t.Fatalf("report=%+v", report)
	}
	if requests.Load() != 0 {
		t.Fatal("shadow contacted control plane")
	}
	if strings.Contains(out.String(), "synthetic-user") {
		t.Fatal("raw input leaked into report")
	}
}

func TestPolicyShadowCLIRefusesInvalidInputsWithoutReport(t *testing.T) {
	for _, name := range []string{"missing", "trailing-argument", "bad-time", "expired", "wrong-issuer", "wrong-key-id", "bad-public-key", "bad-signature", "unknown-json", "trailing-json", "oversized"} {
		t.Run(name, func(t *testing.T) {
			args, directory := shadowCLIInputs(t)
			switch name {
			case "missing":
				args = []string{"policy-shadow"}
			case "trailing-argument":
				args = append(args, "extra")
			case "bad-time":
				args[len(args)-1] = "not-a-time"
			case "expired":
				args[len(args)-1] = "2027-01-01T00:00:00Z"
			case "wrong-issuer":
				args[8] = "wrong-issuer"
			case "wrong-key-id":
				args[10] = "wrong-key"
			case "bad-public-key":
				if err := os.WriteFile(filepath.Join(directory, "public.key"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad-signature":
				raw, err := os.ReadFile(filepath.Join(directory, "candidate.json"))
				if err != nil {
					t.Fatal(err)
				}
				var bundle policy.Bundle
				if err := json.Unmarshal(raw, &bundle); err != nil {
					t.Fatal(err)
				}
				bundle.Signature.Value = "invalid"
				raw, _ = json.Marshal(bundle)
				if err := os.WriteFile(filepath.Join(directory, "candidate.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-json":
				if err := os.WriteFile(filepath.Join(directory, "cases.json"), []byte(`[{"id":"x","input":{"user_id":"u","unknown":true}}]`), 0600); err != nil {
					t.Fatal(err)
				}
			case "trailing-json":
				file, err := os.OpenFile(filepath.Join(directory, "cases.json"), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString(" {}")
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(filepath.Join(directory, "cases.json"), []byte(strings.Repeat(" ", maximumShadowFileBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var out, errOut bytes.Buffer
			if err := run(context.Background(), args, &out, &errOut); err == nil || out.Len() != 0 {
				t.Fatalf("invalid analysis returned report: err=%v output=%s", err, out.String())
			}
		})
	}
}

func TestPolicyShadowHelpIsSuccessfulAndExplainsLimits(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"policy-shadow", "-h"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "Offline analysis only") || !strings.Contains(errOut.String(), "Changed cases still exit 0") {
		t.Fatalf("help=%s", errOut.String())
	}
}

func TestPolicyShadowCommittedExamples(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"policy-shadow", "-current", "../../examples/policy-shadow/current.json", "-candidate", "../../examples/policy-shadow/candidate.json", "-cases", "../../examples/policy-shadow/cases.json", "-issuer", "example://offline-policy-shadow", "-key-id", "synthetic-shadow-key", "-public-key", "../../examples/policy-shadow/public.key", "-at", "2026-10-19T00:00:00Z"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var report policy.ShadowReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.ChangedCases != 1 || len(report.Cases) != 3 || report.Cases[2].Candidate.Decision != domain.DecisionDeny {
		t.Fatalf("unexpected example result: %+v", report)
	}
}
