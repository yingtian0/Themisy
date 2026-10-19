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

	"themisy/internal/grant"
	"themisy/internal/policy"
	policyfiles "themisy/policies"
)

func candidateCLIInputs(t *testing.T, expired bool) ([]string, string, policy.Bundle) {
	t.Helper()
	now := time.Now().UTC()
	signer, public, err := grant.NewDevelopmentSigner("candidate-test-key")
	if err != nil {
		t.Fatal(err)
	}
	source, err := policyfiles.FS.ReadFile("baseline/main.rego")
	if err != nil {
		t.Fatal(err)
	}
	issued, expires := now.Add(-time.Minute), now.Add(time.Hour)
	if expired {
		issued, expires = now.Add(-2*time.Hour), now.Add(-time.Hour)
	}
	bundle, err := policy.SignBundle(context.Background(), signer, policy.Bundle{
		Version: "test-v1", Issuer: "test-local-candidate", Compatibility: []string{policy.InputVersionV1Alpha1},
		IssuedAt: issued, ExpiresAt: expires, Modules: []policy.Module{{Name: "baseline.rego", Layer: policy.LayerPlatform, Source: string(source)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "catalog"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "public.key"), []byte(base64.RawURLEncoding.EncodeToString(public)), 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"-directory", filepath.Join(dir, "catalog"), "-issuer", bundle.Issuer, "-key-id", "candidate-test-key", "-public-key", filepath.Join(dir, "public.key")}, dir, bundle
}

func TestPolicyCandidateCLIRegisterGetWithoutServer(t *testing.T) {
	common, directory, bundle := candidateCLIInputs(t, false)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	for _, action := range []string{"register", "get", "register"} {
		args := append([]string{"-server", server.URL, "policy-candidate", action}, common...)
		if action == "register" {
			args = append(args, "-bundle", filepath.Join(directory, "bundle.json"))
		} else {
			args = append(args, "-hash", bundle.Hash)
		}
		var out, stderr bytes.Buffer
		if err := run(context.Background(), args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Mode   string         `json:"mode"`
			Hash   string         `json:"hash"`
			Bundle *policy.Bundle `json:"bundle"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Mode != "LOCAL_CANDIDATE_ONLY" || result.Hash != bundle.Hash {
			t.Fatalf("result=%+v", result)
		}
		if action == "get" && (result.Bundle == nil || result.Bundle.Signature != bundle.Signature) {
			t.Fatal("get lost signed Bundle")
		}
		if action == "register" && result.Bundle != nil {
			t.Fatal("unexpected registration body")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("candidate command contacted server")
	}
	entries, err := os.ReadDir(filepath.Join(directory, "catalog"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("catalog=%v %v", entries, err)
	}
}

func TestPolicyCandidateCLIFailuresHaveNoReport(t *testing.T) {
	for _, kind := range []string{"expired", "clock-override", "missing", "wrong-key", "wrong-issuer", "trailing", "tamper", "unknown", "both", "missing-directory", "hash-path", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			common, directory, bundle := candidateCLIInputs(t, kind == "expired")
			args := append([]string{"policy-candidate", "register"}, common...)
			args = append(args, "-bundle", filepath.Join(directory, "bundle.json"))
			ctx := context.Background()
			switch kind {
			case "clock-override":
				args = append(args, "-at", "2026-10-19T00:00:00Z")
			case "missing":
				args = []string{"policy-candidate", "register"}
			case "wrong-key":
				for i := range args {
					if args[i] == "candidate-test-key" {
						args[i] = "unknown"
					}
				}
			case "wrong-issuer":
				for i := range args {
					if args[i] == bundle.Issuer {
						args[i] = "wrong"
					}
				}
			case "trailing":
				args = append(args, "extra")
			case "tamper", "unknown":
				path := filepath.Join(directory, "bundle.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "tamper" {
					data = []byte(strings.Replace(string(data), "test-v1", "forged", 1))
				} else {
					data = append([]byte(`{"extra":true,`), data[1:]...)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "both":
				args = append(args, "-hash", bundle.Hash)
			case "missing-directory":
				if err := os.Remove(filepath.Join(directory, "catalog")); err != nil {
					t.Fatal(err)
				}
			case "hash-path":
				args = append(append([]string{"policy-candidate", "get"}, common...), "-hash", "../../outside")
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var out, stderr bytes.Buffer
			if err := run(ctx, args, &out, &stderr); err == nil {
				t.Fatal("invalid command succeeded")
			}
			if out.Len() != 0 {
				t.Fatalf("error emitted success report: %s", out.String())
			}
		})
	}
}

func TestPolicyCandidateCLIHelp(t *testing.T) {
	for _, args := range [][]string{{"policy-candidate", "-h"}, {"policy-candidate", "register", "-h"}, {"policy-candidate", "get", "-h"}} {
		var out, stderr bytes.Buffer
		if err := run(context.Background(), args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || !strings.Contains(stderr.String(), "Usage:") {
			t.Fatal("unexpected help output")
		}
	}
}
