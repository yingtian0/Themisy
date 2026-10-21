package contextsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/pkg/adapter"
)

type ciAuditSink struct {
	events []domain.AuditEvent
	err    error
	hook   func(context.Context, domain.AuditEvent)
}

func (s *ciAuditSink) AppendAuditContext(ctx context.Context, event domain.AuditEvent) error {
	s.events = append(s.events, event)
	if s.hook != nil {
		s.hook(ctx, event)
	}
	return s.err
}

func TestAuditedCICollectionRecordsReproducibleFourValuedObservations(t *testing.T) {
	for _, status := range []adapter.VerificationStatus{adapter.VerificationPass, adapter.VerificationFail, adapter.VerificationMissing, adapter.VerificationInconclusive} {
		t.Run(string(status), func(t *testing.T) {
			collector, _, source, request, _, now := ciCollectorFixture(t)
			switch status {
			case adapter.VerificationFail:
				source.runBody = strings.Replace(source.runBody, `"conclusion":"success"`, `"conclusion":"failure"`, 1)
			case adapter.VerificationMissing:
				source.runBody = `{"total_count":0,"workflow_runs":[]}`
			case adapter.VerificationInconclusive:
				source.err = errors.New("credential-canary raw provider error")
			}
			sink := &ciAuditSink{}
			audited, err := NewAuditedCICollector(collector, sink)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := audited.Collect(context.Background(), request)
			if err != nil || len(sink.events) != 1 {
				t.Fatalf("events=%d err=%v", len(sink.events), err)
			}
			event := sink.events[0]
			record, ok := event.Details["record"].(CIRecord)
			if !ok || !reflect.DeepEqual(record, snapshot.Record()) || event.Result != string(status) || event.ResourceID != snapshot.Hash() || event.Timestamp != record.CapturedAt || event.ID == "" || event.CorrelationID != "context/"+request.TenantID+"/"+event.ID || event.Action != "context.ci.observe" {
				t.Fatalf("observation lost: %+v", event)
			}
			raw, err := json.Marshal(event)
			if err != nil || strings.Contains(string(raw), "credential-canary") || strings.Contains(string(raw), "raw provider error") {
				t.Fatal("audit contains raw error/credential or is not serializable")
			}
			// Audit JSON can reproduce the content hash, but cannot mint trust.
			var stored struct {
				Details struct {
					Record CIRecord `json:"record"`
				} `json:"details"`
			}
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			hash, err := hashRecord(stored.Details.Record)
			if err != nil || hash != snapshot.Hash() {
				t.Fatal("audit JSON cannot reproduce snapshot hash")
			}
			recordJSON, _ := json.Marshal(stored.Details.Record)
			var imported CISnapshot
			if err := json.Unmarshal(recordJSON, &imported); !errors.Is(err, ErrUntrustedCIRecord) || imported.RequireFreshCI(now) == nil {
				t.Fatal("audit record became trusted input")
			}
			if (snapshot.RequireFreshCI(now) == nil) != (status == adapter.VerificationPass) {
				t.Fatal("audit changed evidence meaning")
			}
		})
	}
}

func TestAuditedCICollectionDiscardsResultOnAmbiguousAppendOrCancellation(t *testing.T) {
	for _, kind := range []string{"append-failed", "response-lost", "cancelled", "append-deadline"} {
		t.Run(kind, func(t *testing.T) {
			collector, _, _, request, _, _ := ciCollectorFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sink := &ciAuditSink{}
			switch kind {
			case "append-failed", "response-lost":
				sink.err = errors.New("credential-canary backend unavailable")
			case "cancelled":
				sink.hook = func(context.Context, domain.AuditEvent) { cancel() }
			case "append-deadline":
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, time.Second)
				defer deadlineCancel()
				sink.hook = func(appendCtx context.Context, _ domain.AuditEvent) {
					deadline, ok := appendCtx.Deadline()
					if !ok || time.Until(deadline) > 5*time.Second {
						t.Error("audit append has no bounded deadline")
					}
					<-appendCtx.Done()
				}
			}
			audited, _ := NewAuditedCICollector(collector, sink)
			snapshot, err := audited.Collect(ctx, request)
			if !errors.Is(err, ErrCIAuditUnavailable) || !reflect.DeepEqual(snapshot, CISnapshot{}) || len(sink.events) != 1 {
				t.Fatalf("ambiguous append returned data or retried: events=%d err=%v", len(sink.events), err)
			}
		})
	}
}

func TestAuditedCICollectionDoesNotReusePriorCommittedPass(t *testing.T) {
	collector, credentials, _, request, _, now := ciCollectorFixture(t)
	sink := &ciAuditSink{}
	audited, _ := NewAuditedCICollector(collector, sink)
	first, err := audited.Collect(context.Background(), request)
	if err != nil || first.RequireFreshCI(now) != nil {
		t.Fatal("first committed observation failed")
	}
	sink.err = errors.New("append acknowledgement lost")
	second, err := audited.Collect(context.Background(), request)
	if !errors.Is(err, ErrCIAuditUnavailable) || second.Hash() != "" || len(sink.events) != 2 || credentials.calls != 2 {
		t.Fatal("failed new append reused prior committed observation")
	}
	if sink.events[0].ID == sink.events[1].ID || sink.events[0].CorrelationID == sink.events[1].CorrelationID {
		t.Fatal("independent collection attempts reused audit identity")
	}
}

func TestAuditedCICollectionCannotExtendFreshnessOrMutateSnapshot(t *testing.T) {
	collector, _, _, request, _, now := ciCollectorFixture(t)
	sink := &ciAuditSink{hook: func(_ context.Context, event domain.AuditEvent) {
		record := event.Details["record"].(CIRecord)
		record.Evidence.Artifacts[0].Digest = "changed by sink"
		event.Details["record"] = record
		// Expire CI during the append. A committed observation is still stale.
		now = now.Add(time.Hour)
	}}
	audited, _ := NewAuditedCICollector(collector, sink)
	snapshot, err := audited.Collect(context.Background(), request)
	if err != nil || snapshot.Record().Evidence.Artifacts[0].Digest == "changed by sink" || snapshot.RequireFreshCI(now) == nil {
		t.Fatal("audit mutation or latency changed snapshot/freshness")
	}
}

func TestAuditedCICollectionRejectsIncompleteCompositionAndInvalidRequests(t *testing.T) {
	collector, credential, source, request, _, _ := ciCollectorFixture(t)
	sink := &ciAuditSink{}
	for _, c := range []*CICollector{nil, {}} {
		if _, err := NewAuditedCICollector(c, sink); !errors.Is(err, ErrCIAuditUnavailable) {
			t.Fatal("invalid collector accepted")
		}
	}
	if _, err := NewAuditedCICollector(collector, nil); !errors.Is(err, ErrCIAuditUnavailable) {
		t.Fatal("missing audit accepted")
	}
	var missing *AuditedCICollector
	if snapshot, err := missing.Collect(context.Background(), request); !errors.Is(err, ErrCIAuditUnavailable) || snapshot.Hash() != "" {
		t.Fatal("nil collector returned observation")
	}
	audited, _ := NewAuditedCICollector(collector, sink)
	request.TenantID = "unmapped"
	if snapshot, err := audited.Collect(context.Background(), request); !errors.Is(err, ErrCIContextUnavailable) || snapshot.Hash() != "" || len(sink.events) != 0 || credential.calls != 0 || len(source.requests) != 0 {
		t.Fatal("invalid request performed source/audit IO")
	}
}
