//go:build integration

package policy

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"themisy/internal/domain"
	"themisy/internal/store/postgres"
)

type durableShadowSink struct {
	append func(context.Context, domain.AuditEvent) error
	events []domain.AuditEvent
}

func (s *durableShadowSink) AppendAuditContext(ctx context.Context, event domain.AuditEvent) error {
	s.events = append(s.events, event)
	return s.append(ctx, event)
}
func TestPostgresShadowAudit(t *testing.T) {
	databaseURL := os.Getenv("THEMISY_SHADOW_AUDIT_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("dedicated shadow audit fixture not configured")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Path != "/themisy_shadow_audit_fixture" || u.Port() == "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
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
		t.Fatal("fixture must be fresh; existing schema not modified")
	}
	if err := postgres.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	pg, err := postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	current, signer, keys, now := shadowFixture(t)
	candidate := signShadowCandidate(t, signer, current, `package themisy.authorization
import rego.v1
deny contains "SERVICE_FREEZE" if {input.service == "frozen"}`)
	sink := &durableShadowSink{append: pg.AppendAuditContext}
	comparator, err := NewAuditedShadowComparator(current.Issuer, keys, sink)
	if err != nil {
		t.Fatal(err)
	}
	comparator.now = func() time.Time { return now }
	cases := []ShadowCase{{ID: "frozen", Input: Input{UserID: "private-canary", AgentID: "agent", Service: "frozen", Environment: domain.EnvironmentStaging}}}
	record, err := comparator.Compare(ctx, current, candidate, cases)
	if err != nil {
		t.Fatal(err)
	}
	pg.Close()
	pg, err = postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	events, err := pg.AuditEvents(sink.events[0].CorrelationID)
	if err != nil || len(events) != 1 {
		t.Fatal("shadow audit reopen", err)
	}
	raw, err := json.Marshal(events[0].Details["record"])
	if err != nil || strings.Contains(string(raw), "private-canary") {
		t.Fatal("raw input stored")
	}
	var stored ShadowAuditRecord
	if json.Unmarshal(raw, &stored) != nil {
		t.Fatal("stored report decode")
	}
	hash, err := shadowReportHash(stored.Report)
	if err != nil || hash != record.Hash || hash != stored.Hash || events[0].ResourceID != hash || stored.Report.ChangedCases != 1 || len(stored.Report.Cases[0].AddedReasons) != 1 {
		t.Fatal("report/hash/differences lost")
	}
	query := u.Query()
	query.Set("default_transaction_read_only", "on")
	u.RawQuery = query.Encode()
	refused, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	rejected := &durableShadowSink{append: refused.AppendAuditContext}
	comparator.sink = rejected
	if record, err := comparator.Compare(ctx, current, candidate, cases); err == nil || record.Hash != "" || len(rejected.events) != 1 {
		t.Fatal("refused append returned report or retried")
	}
	if events, err := pg.AuditEvents(rejected.events[0].CorrelationID); err != nil || len(events) != 0 {
		t.Fatal("refused INSERT persisted")
	}
	t.Log("Shadow report differences and hash survive PostgreSQL reopen; refused INSERT returns no report")
}
