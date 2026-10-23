package policy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"themisy/internal/domain"
	"time"
)

type shadowSink struct {
	events []domain.AuditEvent
	err    error
	hook   func(context.Context, domain.AuditEvent)
}

func (s *shadowSink) AppendAuditContext(ctx context.Context, e domain.AuditEvent) error {
	s.events = append(s.events, e)
	if s.hook != nil {
		s.hook(ctx, e)
	}
	return s.err
}
func TestAuditedShadowPreservesComparisonAndIndependentRecords(t *testing.T) {
	current, signer, keys, now := shadowFixture(t)
	candidate := signShadowCandidate(t, signer, current, `package themisy.authorization
import rego.v1
deny contains "SERVICE_FREEZE" if {input.service == "frozen"}`)
	cases := []ShadowCase{{ID: "frozen-case", Input: Input{UserID: "private-user-canary", AgentID: "agent", Service: "frozen", Environment: domain.EnvironmentStaging}}}
	sink := &shadowSink{}
	c, err := NewAuditedShadowComparator(current.Issuer, keys, sink)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	record, err := c.Compare(context.Background(), current, candidate, cases)
	if err != nil || len(sink.events) != 1 || record.Report.ChangedCases != 1 {
		t.Fatal("comparison lost", err)
	}
	event := sink.events[0]
	stored := event.Details["record"].(ShadowAuditRecord)
	hash, _ := shadowReportHash(stored.Report)
	if hash != record.Hash || hash != stored.Hash || event.ResourceID != hash || event.Result != "OBSERVED" || event.Action != "policy.shadow.observe" {
		t.Fatal("audit binding lost")
	}
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), "private-user-canary") || strings.Contains(string(raw), "input.service") {
		t.Fatal("raw input/policy stored")
	}
	record.Report.Cases[0].Candidate.Reasons[0] = "caller-change"
	if stored.Report.Cases[0].Candidate.Reasons[0] == "caller-change" {
		t.Fatal("caller aliases audit")
	}
	sink.hook = func(_ context.Context, e domain.AuditEvent) {
		r := e.Details["record"].(ShadowAuditRecord)
		r.Report.Cases[0].Current.Reasons[0] = "sink-change"
	}
	again, err := c.Compare(context.Background(), current, candidate, cases)
	if err != nil || again.Report.Cases[0].Current.Reasons[0] == "sink-change" {
		t.Fatal("sink aliases return", err)
	}
	// Zero differences is an observation, never a promotion approval.
	sink.hook = nil
	equal, err := c.Compare(context.Background(), current, current, cases)
	if err != nil || equal.Report.ChangedCases != 0 || sink.events[len(sink.events)-1].Result != "OBSERVED" {
		t.Fatal("unchanged report treated as approval")
	}
}
func TestAuditedShadowDiscardsReportOnFailureAndExpiry(t *testing.T) {
	for _, mode := range []string{"signature", "audit", "cancel-before", "cancel-append", "expire-append", "clock-reversal"} {
		t.Run(mode, func(t *testing.T) {
			current, _, keys, now := shadowFixture(t)
			candidate := current
			cases := []ShadowCase{{ID: "case", Input: Input{UserID: "user", AgentID: "agent", Service: "service", Environment: domain.EnvironmentStaging}}}
			sink := &shadowSink{}
			c, _ := NewAuditedShadowComparator(current.Issuer, keys, sink)
			c.now = func() time.Time { return now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "signature":
				candidate.Signature.Value = "invalid"
			case "audit":
				sink.err = errors.New("response lost secret-canary")
			case "cancel-before":
				cancel()
			case "cancel-append":
				sink.hook = func(context.Context, domain.AuditEvent) { cancel() }
			case "expire-append":
				sink.hook = func(context.Context, domain.AuditEvent) { c.now = func() time.Time { return current.ExpiresAt } }
			case "clock-reversal":
				sink.hook = func(context.Context, domain.AuditEvent) { c.now = func() time.Time { return now.Add(-time.Second) } }
			}
			record, err := c.Compare(ctx, current, candidate, cases)
			if !errors.Is(err, ErrShadowAudit) || record.Hash != "" || len(record.Report.Cases) != 0 {
				t.Fatal("failed comparison returned report", err)
			}
			if (mode == "signature" || mode == "cancel-before") && len(sink.events) != 0 {
				t.Fatal("unverified comparison audited")
			}
			if len(sink.events) > 1 {
				t.Fatal("ambiguous append retried")
			}
		})
	}
}
