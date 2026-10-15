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
	runnercore "themisy/internal/runner"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/credentials"
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

func TestPostgresThreeWayRecoveryMismatchIsAudited(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	databaseURL := os.Getenv("THEMISY_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://themisy:themisy@localhost:5432/themisy?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := postgresstore.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	if err := postgresstore.MigrateRunner(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	control, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if control != nil {
			control.Close()
		}
	}()
	journal, err := postgresstore.NewRunnerJournal(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if journal != nil {
			journal.Close()
		}
	}()
	now := time.Now().UTC().Truncate(time.Microsecond)
	run := approvedTransportRun(now)
	run.ID = fmt.Sprintf("three-way-%d", time.Now().UnixNano())
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
	hash, err := protocol.GrantHash(issued.Grant)
	if err != nil {
		t.Fatal(err)
	}
	// The Control Plane already has a terminal observation that disagrees with
	// the later read-only observation returned by the external runtime.
	controlResult := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: issued.Grant.GrantID, RunID: run.ID, StepID: "payment-api", Status: protocol.ResultSucceeded, ExternalExecutionID: "control-plane-deployment", CompletedAt: now}
	if err := control.ReconcileGrantDispatch(ctx, issued.Grant.GrantID, issued.StateVersion, controlResult, domain.AuditEvent{ID: run.ID + "-cp-result", CorrelationID: run.ID, Action: "grant.reconcile", Result: "RECONCILED", Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	record, created, err := journal.ReserveRunnerAction(ctx, store.RunnerActionRecord{GrantID: issued.Grant.GrantID, RunID: run.ID, StepID: "payment-api", TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup, Nonce: issued.Grant.Nonce, IdempotencyKey: issued.Grant.IdempotencyKey, RequestHash: hash, Target: issued.Grant.Target, Action: issued.Grant.Action, RunnerID: "original", CreatedAt: now, UpdatedAt: now}, domain.AuditEvent{ID: run.ID + "-reserve", CorrelationID: run.ID, Action: "runner.action.reserve", Result: "AUTHORIZED", Timestamp: now})
	if err != nil || !created {
		t.Fatalf("reserve created=%t err=%v", created, err)
	}
	record.Status, record.UpdatedAt = store.RunnerActionDispatched, now
	if err := journal.CompleteRunnerAction(ctx, record, record.StateVersion, domain.AuditEvent{ID: run.ID + "-dispatch", CorrelationID: run.ID, Action: "runner.action.dispatch", Result: "DISPATCHED", Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	auth := StaticRunnerAuthenticator{"reconciler": {TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup, Token: "secret"}}
	server := httptest.NewServer((&RunnerServer{Store: control, Audit: control, Auth: auth, Now: func() time.Time { return now }}).Handler())
	defer server.Close()
	client := &RunnerClient{BaseURL: server.URL, RunnerID: "reconciler", Token: "secret"}
	observation := "runtime-deployment"
	reads := 0
	reconciler := threeWayReconcilerFunc(func(context.Context, runnercore.AdapterRequest, runnercore.Credential) (runnercore.AdapterResult, bool, error) {
		reads++
		return runnercore.AdapterResult{ExternalExecutionID: observation, CompletedAt: now}, true, nil
	})
	worker := &runnercore.Runner{TenantID: issued.Grant.TenantID, RunnerGroup: issued.Grant.RunnerGroup, RunnerID: "reconciler", Journal: journal, Credentials: threeWayCredentialBroker{credential: credentials.New(map[string][]byte{"token": []byte("read-only")}, now.Add(time.Hour))}, Reporter: client, Now: func() time.Time { return now }}
	if err := worker.Reconcile(ctx, reconciler, 10); err == nil {
		t.Fatal("Control Plane mismatch was not returned to the recovery worker")
	}
	if reads != 1 {
		t.Fatalf("external runtime read count=%d want=1", reads)
	}
	durable, err := journal.GetRunnerAction(ctx, issued.Grant.TenantID, issued.Grant.RunnerGroup, issued.Grant.Nonce)
	if err != nil || durable.Status != store.RunnerActionSucceeded || durable.Result.ExternalExecutionID != observation {
		t.Fatalf("Runner Journal observation=%#v err=%v", durable, err)
	}
	loaded, err := control.GetGrantDispatch(ctx, issued.Grant.GrantID)
	if err != nil || loaded.Status != store.GrantDispatchSucceeded || loaded.Result.ExternalExecutionID != controlResult.ExternalExecutionID {
		t.Fatalf("Control Plane result changed after mismatch: %#v err=%v", loaded, err)
	}
	events, err := control.AuditEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := 0
	for _, event := range events {
		if event.Action == "grant.reconcile" && event.Result == "CONFLICT" && event.ResourceID == issued.Grant.GrantID {
			conflicts++
		}
	}
	if conflicts != 1 {
		t.Fatalf("durable mismatch audit count=%d events=%#v", conflicts, events)
	}
	for _, event := range events {
		if event.Action == "grant.reconcile" && event.Result == "CONFLICT" && event.ResourceID == issued.Grant.GrantID {
			if event.Details["stored_result_status"] != string(protocol.ResultSucceeded) || event.Details["reported_result_status"] != string(protocol.ResultSucceeded) || event.Details["stored_external_execution_id"] != controlResult.ExternalExecutionID || event.Details["reported_external_execution_id"] != observation {
				t.Fatalf("conflict audit does not identify the three-way mismatch: %#v", event.Details)
			}
		}
	}
	server.Close()
	control.Close()
	control = nil
	journal.Close()
	journal = nil
	control, err = postgresstore.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	journal, err = postgresstore.NewRunnerJournal(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	durable, err = journal.GetRunnerAction(ctx, issued.Grant.TenantID, issued.Grant.RunnerGroup, issued.Grant.Nonce)
	if err != nil || durable.Result.ExternalExecutionID != observation {
		t.Fatalf("Runner Journal result did not survive reopen: %#v err=%v", durable, err)
	}
	loaded, err = control.GetGrantDispatch(ctx, issued.Grant.GrantID)
	if err != nil || loaded.Result.ExternalExecutionID != controlResult.ExternalExecutionID {
		t.Fatalf("Control Plane result did not survive reopen: %#v err=%v", loaded, err)
	}
	events, err = control.AuditEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	conflicts = 0
	for _, event := range events {
		if event.Action == "grant.reconcile" && event.Result == "CONFLICT" && event.ResourceID == issued.Grant.GrantID {
			conflicts++
		}
	}
	if conflicts != 1 {
		t.Fatalf("mismatch audit did not survive reopen: %#v", events)
	}
}

type threeWayCredentialBroker struct{ credential credentials.Credential }

func (b threeWayCredentialBroker) Acquire(context.Context, runnercore.CredentialRequest) (runnercore.Credential, error) {
	return b.credential, nil
}

type threeWayReconcilerFunc func(context.Context, runnercore.AdapterRequest, runnercore.Credential) (runnercore.AdapterResult, bool, error)

func (f threeWayReconcilerFunc) Reconcile(ctx context.Context, request runnercore.AdapterRequest, credential runnercore.Credential) (runnercore.AdapterResult, bool, error) {
	return f(ctx, request, credential)
}
