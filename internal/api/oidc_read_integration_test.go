//go:build integration

package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"themisy/internal/application"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/store/postgres"

	"github.com/jackc/pgx/v5"
)

func TestPostgresOIDCReadAPI(t *testing.T) {
	databaseURL := os.Getenv("THEMISY_OIDC_READ_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("dedicated OIDC read fixture not configured")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Path != "/themisy_oidc_read_fixture" || u.Port() == "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("OIDC read test requires the dedicated loopback fixture database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var tables int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname='public'`).Scan(&tables)
	_ = conn.Close(ctx)
	if err != nil || tables != 0 {
		t.Fatal("fixture must be fresh; existing schema is not modified")
	}
	if err := postgres.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	pg, err := postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	seedOIDCReads(t, pg)
	pg.Close()
	pg, err = postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	p, err := planner.New([]domain.Service{{Name: "identity", OwnerTeam: "identity"}})
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeController{}
	app := application.NewReleases(p, pg, controller)
	spy := &oidcReadSpy{OIDCReadModel: app}
	config, key, claims := principalFixture(t)
	sink := &identityAuditRecorder{append: pg.AppendAuditContext}
	api, err := NewOIDCReadAPI(spy, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + principalToken(t, key, claims, "idp-key")}
	for _, path := range []string{"/v1/plans/plan-tenant-a", "/v1/release-runs/run-tenant-a", "/v1/release-runs/run-tenant-a/events", "/v1/approvals", "/v1/runners"} {
		if response := request(t, api.Handler(), http.MethodGet, path, nil, headers); response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/v1/plans/plan-tenant-b", "/v1/release-runs/run-tenant-b", "/v1/release-runs/run-tenant-b/events"} {
		if response := request(t, api.Handler(), http.MethodGet, path, nil, headers); response.Code != http.StatusNotFound {
			t.Fatal("cross-tenant read succeeded")
		}
	}
	for _, event := range sink.events {
		events, err := pg.AuditEvents(event.CorrelationID)
		if err != nil || len(events) != 1 || events[0].ID != event.ID || events[0].Result != "VERIFIED" {
			t.Fatal("read identity admission was not committed")
		}
	}
	q := u.Query()
	q.Set("default_transaction_read_only", "on")
	u.RawQuery = q.Encode()
	readOnly, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	failedSink := &identityAuditRecorder{append: readOnly.AppendAuditContext}
	refused, err := NewOIDCReadAPI(spy, OIDCReadConfig{Identity: config, RequiredRole: "sre"}, failedSink)
	if err != nil {
		t.Fatal(err)
	}
	before := len(spy.tenants)
	if response := request(t, refused.Handler(), http.MethodGet, "/v1/release-runs/run-tenant-a", nil, headers); response.Code != http.StatusUnauthorized || len(spy.tenants) != before || len(failedSink.events) != 1 {
		t.Fatal("read proceeded after refused audit INSERT")
	}
	stored, err := pg.AuditEvents(failedSink.events[0].CorrelationID)
	if err != nil || len(stored) != 0 {
		t.Fatal("refused identity audit persisted")
	}
	if controller.starts != 0 || controller.lastApproval.Actor != "" {
		t.Fatal("read API issued a command")
	}
	t.Log("real PostgreSQL reads isolated both tenants after reopen; audit INSERT refusal invoked zero model reads")
}
