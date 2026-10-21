package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

func TestRedeliveryRefusalDoesNotBlockDurableSuccessRecovery(t *testing.T) {
	for _, refusal := range []string{"before_execute", "executor_rejection"} {
		t.Run(refusal, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			memory, journal := store.NewMemory(), store.NewMemory()
			if _, _, err := memory.CreateRun(approvedTransportRun(now)); err != nil {
				t.Fatal(err)
			}
			signer, _, err := grant.NewDevelopmentSigner("key")
			if err != nil {
				t.Fatal(err)
			}
			issuer, err := application.NewGrants(memory, signer, "https://control.example", 10*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			issued, _, err := issuer.Issue(ctx, application.GrantIssueRequest{RunID: approvedTransportRun(now).ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
			if err != nil {
				t.Fatal(err)
			}
			actionGrant := issued.Grant
			auth := StaticRunnerAuthenticator{
				"original":    {TenantID: actionGrant.TenantID, RunnerGroup: actionGrant.RunnerGroup, Token: "secret"},
				"replacement": {TenantID: actionGrant.TenantID, RunnerGroup: actionGrant.RunnerGroup, Token: "secret"},
			}
			handler := (&RunnerServer{ResultVerifier: testReportVerifier(), Store: memory, Audit: memory, Auth: auth, Lease: time.Minute, Now: func() time.Time { return now }}).Handler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The original write and journal commit succeed, but its result is lost.
				if r.Header.Get(runnerIDHeader) == "original" && strings.HasSuffix(r.URL.Path, "/result") {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			original := &RunnerClient{ResultSigner: testReportSigner(), TenantID: "tenant-1", RunnerGroup: "prod-jp", BaseURL: server.URL, RunnerID: "original", Token: "secret", Wait: time.Millisecond}
			writes := 0
			executor := recoveryExecutorFunc(func(ctx context.Context, g protocol.ActionGrant) (protocol.Result, error) {
				hash, err := protocol.GrantHash(g)
				if err != nil {
					t.Fatal(err)
				}
				record, _, err := journal.ReserveRunnerAction(ctx, store.RunnerActionRecord{GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, TenantID: g.TenantID, RunnerGroup: g.RunnerGroup, Nonce: g.Nonce, IdempotencyKey: g.IdempotencyKey, RequestHash: hash, CreatedAt: now, UpdatedAt: now}, domain.AuditEvent{ID: "reserve", Timestamp: now})
				if err != nil {
					t.Fatal(err)
				}
				writes++
				record.Status = store.RunnerActionSucceeded
				record.Result = protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, Status: protocol.ResultSucceeded, ExternalExecutionID: "original-provider-write", CompletedAt: now}
				if err := journal.CompleteRunnerAction(ctx, record, record.StateVersion, domain.AuditEvent{ID: "complete", Timestamp: now}); err != nil {
					t.Fatal(err)
				}
				return record.Result, nil
			})
			if delivered, err := original.RunOnce(ctx, executor); !delivered || err == nil {
				t.Fatalf("lost report: delivered=%v err=%v", delivered, err)
			}
			now = now.Add(2 * time.Minute)
			replacement := &RunnerClient{ResultSigner: testReportSigner(), TenantID: "tenant-1", RunnerGroup: "prod-jp", BaseURL: server.URL, RunnerID: "replacement", Token: "secret", Wait: time.Millisecond}
			var replacementExecutor GrantExecutor = rejectingProbe{}
			if refusal == "before_execute" {
				replacement.BeforeExecute = func(context.Context, protocol.ActionGrant) error { return grant.ErrWrongTenant }
			} else {
				replacementExecutor = recoveryExecutorFunc(func(_ context.Context, g protocol.ActionGrant) (protocol.Result, error) {
					return protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: g.GrantID, RunID: g.RunID, StepID: g.StepID, Status: protocol.ResultRejected, ReasonCode: "WRONG_TENANT", CompletedAt: now}, errors.New("local tenant mismatch")
				})
			}
			if delivered, err := replacement.RunOnce(ctx, replacementExecutor); !delivered || err != nil {
				t.Fatalf("redelivery: delivered=%v err=%v", delivered, err)
			}
			dispatch, err := memory.GetGrantDispatch(ctx, actionGrant.GrantID)
			if err != nil || dispatch.Status != store.GrantDispatchUnknown || dispatch.Result.ReasonCode != "WRONG_TENANT" {
				t.Errorf("local refusal made global outcome terminal: %#v err=%v", dispatch, err)
			}
			durable, err := journal.GetRunnerAction(ctx, actionGrant.TenantID, actionGrant.RunnerGroup, actionGrant.Nonce)
			if err != nil {
				t.Fatal(err)
			}
			// UNKNOWN must await recovery instead of starting another delivery.
			now = now.Add(2 * time.Minute)
			if _, err := memory.ClaimGrantDispatch(ctx, actionGrant.TenantID, actionGrant.RunnerGroup, "replacement", "another-token", now, now.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refusal was redispatched: %v", err)
			}
			if err := replacement.ReportRecovery(ctx, durable); err != nil {
				t.Fatalf("durable success recovery blocked: %v", err)
			}
			dispatch, err = memory.GetGrantDispatch(ctx, actionGrant.GrantID)
			if err != nil || dispatch.Status != store.GrantDispatchSucceeded || !protocol.ResultEqual(dispatch.Result, durable.Result) || writes != 1 {
				t.Fatalf("dispatch=%#v writes=%d err=%v", dispatch, writes, err)
			}
		})
	}
}

type recoveryExecutorFunc func(context.Context, protocol.ActionGrant) (protocol.Result, error)

func (f recoveryExecutorFunc) Execute(ctx context.Context, g protocol.ActionGrant) (protocol.Result, error) {
	return f(ctx, g)
}
