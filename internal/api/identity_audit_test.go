package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/store"
)

type identityAuditRecorder struct {
	events []domain.AuditEvent
	append func(context.Context, domain.AuditEvent) error
}

func (s *identityAuditRecorder) AppendAuditContext(ctx context.Context, event domain.AuditEvent) error {
	s.events = append(s.events, event)
	if s.append != nil {
		return s.append(ctx, event)
	}
	return nil
}

func TestAuditedOIDCPrincipalCommitsSanitizedDecision(t *testing.T) {
	config, private, claims := principalFixture(t)
	claims["private-custom"] = "raw-claim-canary"
	memory := store.NewMemory()
	sink := &identityAuditRecorder{append: func(ctx context.Context, event domain.AuditEvent) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("audit append not bounded")
		}
		return memory.AppendAuditContext(ctx, event)
	}}
	resolver, err := NewAuditedOIDCIdentityResolver(config, sink)
	if err != nil {
		t.Fatal(err)
	}
	token := principalToken(t, private, claims, "idp-key")
	r := httptest.NewRequest(http.MethodPost, "/?token=query-canary", strings.NewReader(`{"delegation_ref":"unverified-reference-canary"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Actor-ID", "header-canary")
	r.Header.Set("X-Request-ID", "correlation-canary")
	principal, err := resolver.Resolve(r)
	if err != nil || principal.Subject != "verified-approver" || principal.Issuer != config.Verifier.Issuer || principal.ExpiresAt.IsZero() {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
	if len(sink.events) != 1 {
		t.Fatal("unexpected append count")
	}
	event := sink.events[0]
	stored, err := memory.AuditEvents(event.CorrelationID)
	if err != nil || len(stored) != 1 || stored[0].ID != event.ID {
		t.Fatal("decision not committed")
	}
	if event.Action != "identity.resolve" || event.Result != "VERIFIED" || event.ActorID != principal.Subject || event.Details["issuer"] != config.Verifier.Issuer || event.Details["tenant_id"] != "tenant-a" || event.Details["reason_code"] != "OIDC_PRINCIPAL_VERIFIED" {
		t.Fatalf("event=%+v", event)
	}
	principal.Roles[0] = "mutated-result-canary"
	raw, _ := json.Marshal(stored)
	for _, secret := range []string{token, "raw-claim-canary", "query-canary", "header-canary", "correlation-canary", "unverified-reference-canary", "mutated-result-canary"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("untrusted/raw value reached audit: %s", secret)
		}
	}
}

func TestAuditedOIDCPrincipalRejectedDecisionContainsNoUnverifiedSubject(t *testing.T) {
	config, private, claims := principalFixture(t)
	claims["sub"] = "unverified-subject-canary"
	claims["iss"] = "https://untrusted-issuer.example.test"
	sink := &identityAuditRecorder{}
	resolver, err := NewAuditedOIDCIdentityResolver(config, sink)
	if err != nil {
		t.Fatal(err)
	}
	token := principalToken(t, private, claims, "idp-key")
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	principal, err := resolver.Resolve(r)
	if !errors.Is(err, ErrUnauthenticatedPrincipal) || !reflect.DeepEqual(principal, Principal{}) {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
	if len(sink.events) != 1 || sink.events[0].Result != "REJECTED" || sink.events[0].ActorID != "unverified" || sink.events[0].Details["reason_code"] != "OIDC_PRINCIPAL_REJECTED" {
		t.Fatalf("events=%+v", sink.events)
	}
	raw, _ := json.Marshal(sink.events)
	if strings.Contains(string(raw), "unverified-subject-canary") || strings.Contains(string(raw), token) || strings.Contains(string(raw), "https://untrusted-issuer.example.test") {
		t.Fatal("unverified claim or proof persisted")
	}
}

func TestAuditedOIDCPrincipalRefusesUnavailableOrAmbiguousAudit(t *testing.T) {
	for _, kind := range []string{"failed", "persisted-response-lost", "expired-during-append", "cancel-during-append"} {
		t.Run(kind, func(t *testing.T) {
			config, private, claims := principalFixture(t)
			clock := config.Verifier.Now()
			config.Verifier.Now = func() time.Time { return clock }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			memory := store.NewMemory()
			sink := &identityAuditRecorder{append: func(ctx context.Context, event domain.AuditEvent) error {
				if kind != "failed" {
					if err := memory.AppendAuditContext(ctx, event); err != nil {
						return err
					}
				}
				switch kind {
				case "failed", "persisted-response-lost":
					return errors.New("sensitive-driver-error-canary")
				case "expired-during-append":
					clock = clock.Add(2 * time.Hour)
				case "cancel-during-append":
					cancel()
				}
				return nil
			}}
			resolver, err := NewAuditedOIDCIdentityResolver(config, sink)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+principalToken(t, private, claims, "idp-key"))
			principal, err := resolver.Resolve(r)
			if err == nil || !reflect.DeepEqual(principal, Principal{}) || len(sink.events) != 1 {
				t.Fatalf("not fail closed: principal=%+v err=%v appends=%d", principal, err, len(sink.events))
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatal("audit driver details escaped")
			}
			stored, storeErr := memory.AuditEvents(sink.events[0].CorrelationID)
			if storeErr != nil {
				t.Fatal(storeErr)
			}
			if kind == "persisted-response-lost" && len(stored) != 1 {
				t.Fatal("ambiguous committed response fixture not exercised")
			}
		})
	}
	if _, err := NewAuditedOIDCIdentityResolver(OIDCPrincipalConfig{}, nil); !errors.Is(err, ErrIdentityAuditUnavailable) {
		t.Fatal("missing audit sink accepted")
	}
}

func TestAuditFailureStopsApprovalSignal(t *testing.T) {
	service, memory, controller := testApplication(t)
	config, private, claims := principalFixture(t)
	sink := &identityAuditRecorder{append: func(context.Context, domain.AuditEvent) error { return errors.New("audit unavailable") }}
	resolver, err := NewAuditedOIDCIdentityResolver(config, sink)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := &domain.ReleaseRun{ID: "audit-approval", WorkflowID: "audit-approval", RequestID: "audit-request", TenantID: "tenant-a", RequestedBy: "requester", Plan: domain.ReleasePlan{Hash: "plan"}, Status: domain.RunWaitingApproval, StateVersion: 1, CreatedAt: now, UpdatedAt: now, Steps: []domain.ReleaseStep{{Service: "identity", Approval: &domain.Approval{ID: "audit-proof", Status: domain.ApprovalPending, PlanHash: "plan", RequiredRoles: []string{"sre"}, ExpiresAt: now.Add(time.Hour)}}}}
	if _, _, err := memory.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	response := request(t, NewWithIdentity(service, resolver, testLogger()).Handler(), http.MethodPost, "/v1/approvals/audit-proof:approve", nil, map[string]string{"Authorization": "Bearer " + principalToken(t, private, claims, "idp-key"), "X-Tenant-ID": "tenant-a"})
	if response.Code < 400 || controller.lastApproval.Actor != "" || len(sink.events) != 1 {
		t.Fatalf("status=%d signal=%+v", response.Code, controller.lastApproval)
	}
}
