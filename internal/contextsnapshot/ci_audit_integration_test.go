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
	// Dependency collections use the same durable sink but retain their own schema.
	dependencyEvents := []domain.AuditEvent{}
	for _, mode := range []string{"", "fail", "missing", "partial"} {
		collector, _, transport, _, _, key := dependencyFixture(t)
		transport.mode = mode
		collector.audit = sink
		snapshot, err := collector.Collect(ctx, key)
		if err != nil || snapshot.Hash() == "" {
			t.Fatalf("dependency collection: %v", err)
		}
		dependencyEvents = append(dependencyEvents, sink.events[len(sink.events)-1])
	}
	// Keep the existing CI assertions scoped to their schema.
	sink.events = sink.events[:len(sink.events)-len(dependencyEvents)]
	metadataCollector, _, _, _, _, metadataKey, _ := metadataFixture(t)
	metadataCollector.audit = sink
	metadataSnapshot, err := metadataCollector.Collect(ctx, metadataKey)
	if err != nil {
		t.Fatal(err)
	}
	metadataEvent := sink.events[len(sink.events)-1]
	sink.events = sink.events[:len(sink.events)-1]
	ciPart, dependencyPart, metadataPart := sourceContextParts(t, false)
	assembler, err := NewSourceContextAssembler(sink)
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := assembler.Assemble(ctx, ciPart, dependencyPart, metadataPart)
	if err != nil {
		t.Fatal(err)
	}
	sourceContextEvent := sink.events[len(sink.events)-1]
	sink.events = sink.events[:len(sink.events)-1]
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
	for _, event := range dependencyEvents {
		events, err := pg.AuditEvents(event.CorrelationID)
		if err != nil || len(events) != 1 {
			t.Fatalf("dependency audit reopen: %v", err)
		}
		raw, err := json.Marshal(events[0].Details["record"])
		if err != nil || strings.Contains(string(raw), "secret-canary") || strings.Contains(string(raw), "private-query") {
			t.Fatal("invalid dependency audit")
		}
		var record DependencyRecord
		if json.Unmarshal(raw, &record) != nil {
			t.Fatal("dependency record decode")
		}
		hash, err := dependencyHash(record)
		if err != nil || hash != event.ResourceID || hash != record.Hash || len(record.Evidence) != 2 {
			t.Fatal("dependency hash/status binding lost")
		}
		original := event.Details["record"].(DependencyRecord)
		for i := range original.Evidence {
			if original.Evidence[i].Status != record.Evidence[i].Status {
				t.Fatal("dependency status changed")
			}
		}
		var restored DependencySnapshot
		if json.Unmarshal(raw, &restored) == nil || restored.Hash() != "" {
			t.Fatal("database minted dependency trust")
		}
	}
	metadataEvents, err := pg.AuditEvents(metadataEvent.CorrelationID)
	if err != nil || len(metadataEvents) != 1 {
		t.Fatal("metadata audit reopen", err)
	}
	metadataJSON, err := json.Marshal(metadataEvents[0].Details["record"])
	if err != nil || strings.Contains(string(metadataJSON), "credential-canary") {
		t.Fatal("metadata audit serialization")
	}
	var metadataRecord MetadataRecord
	if json.Unmarshal(metadataJSON, &metadataRecord) != nil {
		t.Fatal("metadata audit decode")
	}
	metadataRecordHash, err := metadataHash(metadataRecord)
	if err != nil || metadataRecordHash != metadataSnapshot.Hash() || metadataRecordHash != metadataEvent.ResourceID || metadataRecord.Contract.Revision != metadataSnapshot.Record().Contract.Revision || metadataRecord.Contract.RawHash == "" || metadataRecord.Profile.ContentHash == "" {
		t.Fatal("metadata provenance/hash lost")
	}
	var importedMetadata MetadataSnapshot
	if json.Unmarshal(metadataJSON, &importedMetadata) == nil || importedMetadata.Hash() != "" {
		t.Fatal("database minted metadata trust")
	}
	contextEvents, err := pg.AuditEvents(sourceContextEvent.CorrelationID)
	if err != nil || len(contextEvents) != 1 {
		t.Fatal("assembled source audit reopen", err)
	}
	contextJSON, err := json.Marshal(contextEvents[0].Details["record"])
	if err != nil {
		t.Fatal(err)
	}
	var sourceRecord SourceContextRecord
	if json.Unmarshal(contextJSON, &sourceRecord) != nil {
		t.Fatal("assembled source record decode")
	}
	sourceHash, err := sourceContextHash(sourceRecord)
	if err != nil || sourceHash != assembled.Hash() || sourceHash != sourceContextEvent.ResourceID || sourceRecord.Dependencies == nil || len(sourceRecord.DependencyServices) != 1 || sourceRecord.Metadata.Contract.Revision == sourceRecord.CommitSHA {
		t.Fatal("assembled source bindings lost")
	}
	var restoredContext SourceContextSnapshot
	if json.Unmarshal(contextJSON, &restoredContext) == nil || restoredContext.Hash() != "" {
		t.Fatal("database minted source context trust")
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
	dependencyCollector, _, _, _, _, dependencyKey := dependencyFixture(t)
	dependencyCollector.audit = failedSink
	before := len(failedSink.events)
	if snapshot, err := dependencyCollector.Collect(ctx, dependencyKey); err == nil || snapshot.Hash() != "" || len(failedSink.events) != before+1 {
		t.Fatal("refused dependency INSERT returned a snapshot or retried")
	}
	if events, err := pg.AuditEvents(failedSink.events[before].CorrelationID); err != nil || len(events) != 0 {
		t.Fatal("refused dependency INSERT persisted")
	}
	metadataCollector.audit = failedSink
	before = len(failedSink.events)
	if snapshot, err := metadataCollector.Collect(ctx, metadataKey); err == nil || snapshot.Hash() != "" || len(failedSink.events) != before+1 {
		t.Fatal("refused metadata INSERT returned snapshot or retried")
	}
	if events, err := pg.AuditEvents(failedSink.events[before].CorrelationID); err != nil || len(events) != 0 {
		t.Fatal("refused metadata INSERT persisted")
	}
	assembler.sink = failedSink
	before = len(failedSink.events)
	if snapshot, err := assembler.Assemble(ctx, ciPart, dependencyPart, metadataPart); err == nil || snapshot.Hash() != "" || len(failedSink.events) != before+1 {
		t.Fatal("refused context INSERT returned snapshot or retried")
	}
	if events, err := pg.AuditEvents(failedSink.events[before].CorrelationID); err != nil || len(events) != 0 {
		t.Fatal("refused context INSERT persisted")
	}
	t.Log("PostgreSQL retained four-valued CI records across reopen with reproducible hashes; refused INSERT returned no snapshot")
}
