//go:build integration

package contextsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/store/postgres"
	"themisy/pkg/adapter"

	"github.com/jackc/pgx/v5"
)

type postgresCIAuditSink struct {
	append func(context.Context, domain.AuditEvent) error
	events []domain.AuditEvent
}

func (s *postgresCIAuditSink) AppendAuditContext(ctx context.Context, event domain.AuditEvent) error {
	s.events = append(s.events, event)
	return s.append(ctx, event)
}

func TestPostgresCIObservationAudit(t *testing.T) {
	databaseURL := os.Getenv("THEMISY_CI_AUDIT_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("dedicated CI observation audit database not configured")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Path != "/themisy_ci_audit_fixture" || u.Port() == "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("CI audit test requires the dedicated loopback fixture database")
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
		t.Fatal("CI audit fixture must be fresh; existing schema is not modified")
	}
	if err := postgres.Migrate(databaseURL, false); err != nil {
		t.Fatal(err)
	}
	pg, err := postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sink := &postgresCIAuditSink{append: pg.AppendAuditContext}
	for _, status := range []adapter.VerificationStatus{adapter.VerificationPass, adapter.VerificationFail, adapter.VerificationMissing, adapter.VerificationInconclusive} {
		collector, _, source, request, _, _ := ciCollectorFixture(t)
		switch status {
		case adapter.VerificationFail:
			source.runBody = strings.Replace(source.runBody, `"conclusion":"success"`, `"conclusion":"failure"`, 1)
		case adapter.VerificationMissing:
			source.runBody = `{"total_count":0,"workflow_runs":[]}`
		case adapter.VerificationInconclusive:
			source.err = errors.New("credential-canary raw provider error")
		}
		audited, err := NewAuditedCICollector(collector, sink)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot, err := audited.Collect(ctx, request); err != nil || snapshot.Record().Evidence.Status != status {
			t.Fatalf("collect %s: %v", status, err)
		}
	}
	pg.Close()
	pg, err = postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	for _, event := range sink.events {
		events, err := pg.AuditEvents(event.CorrelationID)
		if err != nil || len(events) != 1 || events[0].ID != event.ID || events[0].Result != event.Result {
			t.Fatalf("persisted audit mismatch: %v", err)
		}
		raw, err := json.Marshal(events[0].Details["record"])
		if err != nil || strings.Contains(string(raw), "credential-canary") {
			t.Fatal("invalid or credential-bearing persisted record")
		}
		var record CIRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		hash, err := hashRecord(record)
		if err != nil || hash != event.ResourceID || record.Hash != hash || record.TenantID != "tenant-a" || record.Evidence.Status != adapter.VerificationStatus(event.Result) {
			t.Fatal("reopened PostgreSQL record lost source/hash/status binding")
		}
		var imported CISnapshot
		if err := json.Unmarshal(raw, &imported); !errors.Is(err, ErrUntrustedCIRecord) || imported.Hash() != "" {
			t.Fatal("database record became a trusted snapshot")
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
	failedSink := &postgresCIAuditSink{append: readOnly.AppendAuditContext}
	collector, _, _, request, _, _ := ciCollectorFixture(t)
	audited, _ := NewAuditedCICollector(collector, failedSink)
	if snapshot, err := audited.Collect(ctx, request); !errors.Is(err, ErrCIAuditUnavailable) || snapshot.Hash() != "" || len(failedSink.events) != 1 {
		t.Fatal("refused PostgreSQL INSERT returned a snapshot or retried")
	}
	stored, err := pg.AuditEvents(failedSink.events[0].CorrelationID)
	if err != nil || len(stored) != 0 {
		t.Fatal("refused transaction persisted an audit")
	}
	t.Log("PostgreSQL retained four-valued CI records across reopen with reproducible hashes; refused INSERT returned no snapshot")
}
