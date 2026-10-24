//go:build integration

package verification

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/store/postgres"
	"time"
)

func TestPostgresRuntimeAudit(t *testing.T) {
	databaseURL := os.Getenv("THEMISY_RUNTIME_AUDIT_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("dedicated runtime audit fixture not configured")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Path != "/themisy_runtime_audit_fixture" || u.Port() == "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("dedicated loopback fixture required")
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
		t.Fatal("fresh fixture required; existing database not changed")
	}
	if err := postgres.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	pg, err := postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	collector, client, _, _, request := runtimeFixture(t)
	// Keep event identities for exact reopened lookups without broad audit reads.
	recorded := &durableRuntimeSink{append: pg.AppendAuditContext}
	collector.sink = recorded
	if _, err := collector.Collect(ctx, request); err != nil {
		t.Fatal(err)
	}
	client.pending = true
	if _, err := collector.Collect(ctx, request); err != nil {
		t.Fatal(err)
	}
	pg.Close()
	pg, err = postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	for _, event := range recorded.events {
		events, err := pg.AuditEvents(event.CorrelationID)
		if err != nil || len(events) != 1 {
			t.Fatal("runtime audit reopen", err)
		}
		raw, err := json.Marshal(events[0].Details["record"])
		if err != nil || strings.Contains(string(raw), "secret-canary") || strings.Contains(string(raw), "arn:aws:") {
			t.Fatal("raw provider data in audit")
		}
		var record RuntimeRecord
		if json.Unmarshal(raw, &record) != nil {
			t.Fatal("record decode")
		}
		hash, err := runtimeRecordHash(record)
		if err != nil || hash != event.ResourceID || record.Hash != hash || string(record.Observation.Status) != event.Result || record.GrantID != "grant" {
			t.Fatal("runtime hash/status binding lost")
		}
		var imported RuntimeSnapshot
		if json.Unmarshal(raw, &imported) == nil || imported.Hash() != "" {
			t.Fatal("audit record restored trust")
		}
	}
	query := u.Query()
	query.Set("default_transaction_read_only", "on")
	u.RawQuery = query.Encode()
	refused, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	rejected := &durableRuntimeSink{append: refused.AppendAuditContext}
	collector.sink = rejected
	if snapshot, err := collector.Collect(ctx, request); err == nil || snapshot.Hash() != "" || len(rejected.events) != 1 {
		t.Fatal("refused INSERT returned snapshot or retried")
	}
	if events, err := pg.AuditEvents(rejected.events[0].CorrelationID); err != nil || len(events) != 0 {
		t.Fatal("refused INSERT persisted")
	}
	if client.writes != 0 {
		t.Fatal("observation wrote ECS")
	}
}

type durableRuntimeSink struct {
	append func(context.Context, domain.AuditEvent) error
	events []domain.AuditEvent
}

func (s *durableRuntimeSink) AppendAuditContext(ctx context.Context, event domain.AuditEvent) error {
	s.events = append(s.events, event)
	return s.append(ctx, event)
}
