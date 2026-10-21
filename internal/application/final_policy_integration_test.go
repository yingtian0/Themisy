//go:build integration

package application

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/internal/store"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/protocol"
)

func TestPostgresFinalPolicyIssuanceAtomicity(t *testing.T) {
	url := os.Getenv("THEMISY_FINAL_POLICY_TEST_DATABASE_URL")
	if url == "" && os.Getenv("THEMISY_INTEGRATION") == "1" {
		url = os.Getenv("THEMISY_DATABASE_URL")
		if url == "" {
			url = "postgres://themisy:themisy@localhost:5432/themisy?sslmode=disable"
		}
	}
	if url == "" {
		t.Skip("requires dedicated disposable final-policy database")
	}
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	st, err := postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, changeDuringSign := range []bool{false, true} {
		now := time.Now().UTC()
		run := grantRun(now)
		run.ID += "-" + now.Format("20060102150405.000000000")
		run.RequestID, run.WorkflowID = run.ID, run.ID
		if _, _, err := st.CreateRun(run); err != nil {
			t.Fatal(err)
		}
		signer, _, _ := grant.NewDevelopmentSigner("key")
		var signing grant.Signer = signer
		if changeDuringSign {
			signing = updateWhileSigning{Signer: signer, update: func() {
				current, err := st.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				current.Steps[1].Approval.Status = domain.ApprovalRevoked
				if err := st.UpdateRun(current, current.StateVersion); err != nil {
					t.Fatal(err)
				}
			}}
		}
		issuer, err := NewGrants(st, signing, "https://control.example", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		issuer.Now = func() time.Time { return now }
		result, _, err := issuer.Issue(context.Background(), GrantIssueRequest{RunID: run.ID, StepID: "payment-api", Capability: protocol.CapabilityDeploy})
		events, readErr := st.AuditEvents(run.ID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if changeDuringSign {
			if !errors.Is(err, store.ErrConflict) || len(events) != 0 {
				t.Fatalf("stale issue err=%v events=%d", err, len(events))
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Details["policy_input_hash"] != result.Grant.PolicyInputHash || events[0].Details["final_policy_decision"] == nil {
			t.Fatalf("missing durable final decision: %+v", events)
		}
		if _, err := st.GetGrantDispatch(context.Background(), result.Grant.GrantID); err != nil {
			t.Fatal(err)
		}
	}
}
