package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"themisy/internal/domain"
)

type client struct {
	baseURL string
	tenant  string
	actor   string
	roles   string
	http    *http.Client
}

// Set only by the source-pinned packaging build; ordinary source builds say dev.
var version = "dev"
var sourceCommit = "unknown"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "themisyctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("themisyctl", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", envOr("THEMISY_SERVER", "http://localhost:8080"), "Themisy control-plane base URL")
	tenant := flags.String("tenant", envOr("THEMISY_TENANT_ID", "default"), "authenticated tenant")
	actor := flags.String("actor", os.Getenv("THEMISY_ACTOR_ID"), "authenticated actor for control operations")
	roles := flags.String("roles", os.Getenv("THEMISY_ACTOR_ROLES"), "comma-separated trusted actor roles")
	if err := flags.Parse(args); err != nil {
		return err
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		return errors.New("command is required: version, offline-plan, services, plan, get-plan, start, status, events, pause, resume, cancel, approvals, approve, deny, revoke, runners, freeze-runner, validate-contract")
	}
	command, values := remaining[0], remaining[1:]
	if command == "version" {
		if len(values) != 0 {
			return errors.New("version accepts no arguments")
		}
		return json.NewEncoder(stdout).Encode(struct {
			Version string `json:"version"`
			Commit  string `json:"source_commit"`
		}{version, sourceCommit})
	}
	if command == "offline-plan" {
		return runOfflinePlan(values, stdout, stderr)
	}
	c := &client{baseURL: strings.TrimRight(*server, "/"), tenant: *tenant, actor: *actor, roles: *roles, http: &http.Client{Timeout: 30 * time.Second}}
	if requiresOneArgument(command) && len(values) != 1 {
		return fmt.Errorf("%s requires exactly one argument", command)
	}
	var data []byte
	var err error
	switch command {
	case "services":
		data, err = c.request(ctx, http.MethodGet, "/v1/services", nil, false)
	case "plan":
		data, err = c.fileRequest(ctx, http.MethodPost, "/v1/plans", oneArg(values), false)
	case "get-plan":
		data, err = c.request(ctx, http.MethodGet, "/v1/plans/"+escape(oneArg(values)), nil, false)
	case "start":
		data, err = c.fileRequest(ctx, http.MethodPost, "/v1/release-runs", oneArg(values), false)
	case "status":
		data, err = c.request(ctx, http.MethodGet, "/v1/release-runs/"+escape(oneArg(values)), nil, false)
	case "events":
		data, err = c.request(ctx, http.MethodGet, "/v1/release-runs/"+escape(oneArg(values))+"/events", nil, false)
	case "pause", "resume", "cancel":
		data, err = c.request(ctx, http.MethodPost, "/v1/release-runs/"+escape(oneArg(values))+":"+command, nil, true)
	case "approvals":
		data, err = c.request(ctx, http.MethodGet, "/v1/approvals", nil, false)
	case "approve", "deny", "revoke":
		data, err = c.request(ctx, http.MethodPost, "/v1/approvals/"+escape(oneArg(values))+":"+command, nil, true)
	case "runners":
		data, err = c.request(ctx, http.MethodGet, "/v1/runners", nil, false)
	case "freeze-runner":
		data, err = c.request(ctx, http.MethodPost, "/v1/runners/"+escape(oneArg(values))+":freeze", nil, true)
	case "validate-contract":
		data, err = c.fileRequest(ctx, http.MethodPost, "/v1/contracts:validate", oneArg(values), false)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		return err
	}
	return writePrettyJSON(stdout, data)
}

func (c *client) createPlan(ctx context.Context, input []byte) (domain.ReleasePlan, error) {
	data, err := c.request(ctx, http.MethodPost, "/v1/plans", input, false)
	if err != nil {
		return domain.ReleasePlan{}, err
	}
	var plan domain.ReleasePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return domain.ReleasePlan{}, err
	}
	return plan, nil
}

func (c *client) fileRequest(ctx context.Context, method, path, filename string, authenticated bool) ([]byte, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filename, err)
	}
	return c.request(ctx, method, path, data, authenticated)
}

func (c *client) request(ctx context.Context, method, path string, body []byte, authenticated bool) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Tenant-ID", c.tenant)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		if strings.TrimSpace(c.actor) == "" {
			return nil, errors.New("-actor or THEMISY_ACTOR_ID is required for this command")
		}
		request.Header.Set("X-Actor-ID", c.actor)
		request.Header.Set("X-Actor-Roles", c.roles)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Themisy control plane returned %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func writePrettyJSON(w io.Writer, data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func oneArg(values []string) string {
	return values[0]
}

func requiresOneArgument(command string) bool {
	switch command {
	case "plan", "get-plan", "start", "status", "events", "pause", "resume", "cancel", "approve", "deny", "revoke", "freeze-runner", "validate-contract":
		return true
	default:
		return false
	}
}

func escape(value string) string { return url.PathEscape(value) }

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
