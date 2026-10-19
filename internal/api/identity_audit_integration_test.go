//go:build integration

package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/store/postgres"

	"github.com/jackc/pgx/v5"
)

func TestPostgresOIDCIdentityAudit(t *testing.T) {
	databaseURL := os.Getenv("THEMISY_IDENTITY_AUDIT_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("dedicated identity audit database not configured")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Path != "/themisy_identity_audit_fixture" || u.Port() == "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("identity audit test requires the dedicated loopback fixture database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal("connect dedicated fixture:", err)
	}
	var tables int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname='public'`).Scan(&tables)
	_ = conn.Close(ctx)
	if err != nil || tables != 0 {
		t.Fatal("identity audit fixture must be fresh; existing schema is not modified")
	}
	if err := postgres.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	pg, err := postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config, private, claims := principalFixture(t)
	sink := &identityAuditRecorder{append: pg.AppendAuditContext}
	resolver, err := NewAuditedOIDCIdentityResolver(config, sink)
	if err != nil {
		t.Fatal(err)
	}
	token := principalToken(t, private, claims, "idp-key")
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	if _, err := resolver.Resolve(r); err != nil {
		t.Fatal(err)
	}
	r.Header.Del("Authorization")
	if _, err := resolver.Resolve(r); err == nil {
		t.Fatal("missing identity accepted")
	}
	pg.Close()
	// Reopen the actual store to verify committed data, not recorder state.
	pg, err = postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if len(sink.events) != 2 {
		t.Fatal("expected two audit decisions")
	}
	for i, event := range sink.events {
		events, err := pg.AuditEvents(event.CorrelationID)
		if err != nil || len(events) != 1 || events[0].ID != event.ID {
			t.Fatalf("stored decision %d: %v %v", i, events, err)
		}
		if events[0].Details["tenant_id"] != "tenant-a" || events[0].Details["issuer"] != config.Verifier.Issuer {
			t.Fatal("lost tenant/issuer binding")
		}
		if i == 0 && (events[0].Result != "VERIFIED" || events[0].ActorID != "verified-approver") {
			t.Fatal("verified decision not retained")
		}
		if i == 1 && (events[0].Result != "REJECTED" || events[0].ActorID != "unverified") {
			t.Fatal("rejection not retained")
		}
	}
	// A real server-side read-only transaction refuses audit INSERT. No schema,
	// role or protection setting is changed; this is a separate client session.
	q := u.Query()
	q.Set("default_transaction_read_only", "on")
	u.RawQuery = q.Encode()
	readOnly, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	failedSink := &identityAuditRecorder{append: readOnly.AppendAuditContext}
	failedResolver, err := NewAuditedOIDCIdentityResolver(config, failedSink)
	if err != nil {
		t.Fatal(err)
	}
	service, memory, controller := testApplication(t)
	now := time.Now().UTC()
	run := &domain.ReleaseRun{ID: "pg-audit-approval", WorkflowID: "pg-audit-approval", RequestID: "pg-audit-request", TenantID: "tenant-a", RequestedBy: "requester", Plan: domain.ReleasePlan{Hash: "plan"}, Status: domain.RunWaitingApproval, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Approval: &domain.Approval{ID: "pg-audit-proof", Status: domain.ApprovalPending, PlanHash: "plan", RequiredRoles: []string{"sre"}, ExpiresAt: now.Add(time.Hour)}}}}
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	response := request(t, NewWithIdentity(service, failedResolver, testLogger()).Handler(), http.MethodPost, "/v1/approvals/pg-audit-proof:approve", nil, map[string]string{"Authorization": "Bearer " + token, "X-Tenant-ID": "tenant-a"})
	if response.Code < 400 || controller.lastApproval.Actor != "" || len(failedSink.events) != 1 {
		t.Fatalf("uncommitted audit admitted request: status=%d signal=%+v", response.Code, controller.lastApproval)
	}
	stored, err := pg.AuditEvents(failedSink.events[0].CorrelationID)
	if err != nil || len(stored) != 0 {
		t.Fatalf("refused transaction persisted: %v %v", stored, err)
	}
	t.Log("PostgreSQL retained VERIFIED/REJECTED decisions across reopen; refused INSERT sent zero approval signals")
}
