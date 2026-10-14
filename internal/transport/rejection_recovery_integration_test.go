//go:build integration

package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/protocol"
)

func TestPostgresRedeliveryRefusalDoesNotBlockDurableSuccessRecovery(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	databaseURL := os.Getenv("THEMISY_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://themisy:themisy@localhost:5432/themisy?sslmode=disable"
	}
	if err := postgresstore.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	if err := postgresstore.MigrateRunner(context.Background(), databaseURL); err != nil {
		t.Fatal(err)
	}
	for _, refusal := range []string{"before_execute", "executor_rejection"} {
		t.Run(refusal, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			now := time.Now().UTC().Truncate(time.Microsecond)
			control, err := postgresstore.New(ctx, databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { control.Close() }()
			journal, err := postgresstore.NewRunnerJournal(ctx, databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { journal.Close() }()
			run := approvedTransportRun(now)
			run.ID = fmt.Sprintf("refusal-%s-%d", refusal, time.Now().UnixNano())
			run.RequestID, run.WorkflowID = run.ID, run.ID
			run.Plan.Phases[0].Steps[0].Scheduling.RunnerGroup = run.ID
			if _, _, err := control.CreateRun(run); err != nil {
				t.Fatal(err)
			}
			signer, _, err := grant.NewDevelopmentSigner("key")
			if err != nil {
				t.Fatal(err)
			}
			issuer, err := application.NewGrants(control, signer, "https://control.example", 10*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			issued, _, err := issuer.Issue(ctx, application.GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
			if err != nil {
				t.Fatal(err)
			}
			actionGrant := issued.Grant
			auth := StaticRunnerAuthenticator{
				"original":    {TenantID: actionGrant.TenantID, RunnerGroup: actionGrant.RunnerGroup, Token: "secret"},
				"replacement": {TenantID: actionGrant.TenantID, RunnerGroup: actionGrant.RunnerGroup, Token: "secret"},
			}
			handler := (&RunnerServer{Store: control, Audit: control, Auth: auth, Lease: time.Minute, Now: func() time.Time { return now }}).Handler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The original write and journal commit succeed, but its result is lost.
				if r.Header.Get(runnerIDHeader) == "original" && strings.HasSuffix(r.URL.Path, "/result") {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			original := &RunnerClient{BaseURL: server.URL, RunnerID: "original", Token: "secret", Wait: time.Millisecond}
			writes := 0
			executor := recoveryExecutorFunc(func(ctx context.Context, g protocol.ActionGrant) (protocol.Result, error) {
				hash, err := protocol.GrantHash(g)
				if err != nil {
					t.Fatal(err)
				}
				record, created, err := journal.ReserveRunnerAction(ctx, store.RunnerActionRecord{GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, TenantID: g.TenantID, RunnerGroup: g.RunnerGroup, Nonce: g.Nonce, IdempotencyKey: g.IdempotencyKey, RequestHash: hash, CreatedAt: now, UpdatedAt: now}, domain.AuditEvent{ID: run.ID + "-reserve", CorrelationID: g.GrantID, Action: "runner.action.reserve", Result: "AUTHORIZED", Timestamp: now})
				if err != nil {
					t.Fatal(err)
				}
				if !created {
					t.Fatal("original action was already reserved")
				}
				writes++
				record.Status = store.RunnerActionSucceeded
				record.Result = protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, Status: protocol.ResultSucceeded, ExternalExecutionID: "original-provider-write", CompletedAt: now}
				if err := journal.CompleteRunnerAction(ctx, record, record.StateVersion, domain.AuditEvent{ID: run.ID + "-complete", CorrelationID: g.GrantID, Action: "runner.action.complete", Result: "SUCCEEDED", Timestamp: now}); err != nil {
					t.Fatal(err)
				}
				return record.Result, nil
			})
			if delivered, err := original.RunOnce(ctx, executor); !delivered || err == nil {
				t.Fatalf("lost report: delivered=%v err=%v", delivered, err)
			}
			now = now.Add(2 * time.Minute)
			replacement := &RunnerClient{BaseURL: server.URL, RunnerID: "replacement", Token: "secret", Wait: time.Millisecond}
			replacementCalls := 0
			var replacementExecutor GrantExecutor = recoveryExecutorFunc(func(context.Context, protocol.ActionGrant) (protocol.Result, error) {
				replacementCalls++
				return protocol.Result{}, errors.New("unexpected execution after BeforeExecute refusal")
			})
			if refusal == "before_execute" {
				replacement.BeforeExecute = func(context.Context, protocol.ActionGrant) error { return grant.ErrWrongTenant }
			} else {
				replacementExecutor = recoveryExecutorFunc(func(_ context.Context, g protocol.ActionGrant) (protocol.Result, error) {
					replacementCalls++
					return protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, Status: protocol.ResultRejected, ReasonCode: "WRONG_TENANT", CompletedAt: now}, errors.New("local tenant mismatch")
				})
			}
			if delivered, err := replacement.RunOnce(ctx, replacementExecutor); !delivered || err != nil {
				t.Fatalf("redelivery: delivered=%v err=%v", delivered, err)
			}
			wantCalls := 0
			if refusal == "executor_rejection" {
				wantCalls = 1
			}
			if replacementCalls != wantCalls {
				t.Fatalf("replacement executor calls=%d want=%d", replacementCalls, wantCalls)
			}
			dispatch, err := control.GetGrantDispatch(ctx, actionGrant.GrantID)
			if err != nil || dispatch.Status != store.GrantDispatchUnknown || dispatch.Result.ReasonCode != "WRONG_TENANT" {
				t.Errorf("local refusal made global outcome terminal: %#v err=%v", dispatch, err)
			}
			// Reopen both persistence clients before reading saved success and recovering.
			server.Close()
			control.Close()
			journal.Close()
			control, err = postgresstore.New(ctx, databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			journal, err = postgresstore.NewRunnerJournal(ctx, databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			server = httptest.NewServer((&RunnerServer{Store: control, Audit: control, Auth: auth, Lease: time.Minute, Now: func() time.Time { return now }}).Handler())
			defer server.Close()
			replacement.BaseURL = server.URL
			durable, err := journal.GetRunnerAction(ctx, actionGrant.TenantID, actionGrant.RunnerGroup, actionGrant.Nonce)
			if err != nil {
				t.Fatal(err)
			}
			dispatch, err = control.GetGrantDispatch(ctx, actionGrant.GrantID)
			if err != nil || dispatch.Status != store.GrantDispatchUnknown {
				t.Fatalf("UNKNOWN did not survive reopen: %#v err=%v", dispatch, err)
			}
			// UNKNOWN must await recovery instead of starting another delivery.
			now = now.Add(2 * time.Minute)
			if _, err := control.ClaimGrantDispatch(ctx, actionGrant.TenantID, actionGrant.RunnerGroup, "replacement", "another-token", now, now.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refusal was redispatched: %v", err)
			}
			if err := replacement.ReportRecovery(ctx, durable); err != nil {
				t.Fatalf("durable success recovery blocked: %v", err)
			}
			// A second reopen proves the recovered dispatch and audit are durable.
			control.Close()
			control, err = postgresstore.New(ctx, databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			events, err := control.AuditEvents(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			reconciled, refused := false, false
			for _, event := range events {
				if event.Action == "grant.result" && event.Result == string(protocol.ResultUnknown) && event.Details["reason_code"] == "WRONG_TENANT" {
					refused = true
				}
				if event.Action == "grant.reconcile" && event.Result == "RECONCILED" {
					reconciled = true
				}
			}
			if !reconciled || !refused {
				t.Fatalf("missing durable recovery audit: %#v", events)
			}
			journalEvents, err := journal.AuditEvents(actionGrant.GrantID)
			if err != nil || len(journalEvents) != 2 {
				t.Fatalf("journal audit=%#v err=%v", journalEvents, err)
			}
			dispatch, err = control.GetGrantDispatch(ctx, actionGrant.GrantID)
			if err != nil || dispatch.Status != store.GrantDispatchSucceeded || dispatch.Result != durable.Result || writes != 1 {
				t.Fatalf("dispatch=%#v writes=%d err=%v", dispatch, writes, err)
			}
		})
	}
}
