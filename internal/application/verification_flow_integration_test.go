//go:build integration

package application

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"themisy/internal/resultreport"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/adapter"
	"themisy/pkg/protocol"
)

func TestPostgresSignedObservationFlow(t *testing.T) {
	url := os.Getenv("THEMISY_OBSERVATION_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires dedicated observation fixture database")
	}
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	control, err := postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err := postgresstore.MigrateRunner(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	journal, err := postgresstore.NewRunnerJournal(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	receipt := testObservationFlow(t, control, journal)
	control.Close()
	journal.Close()
	reopened, err := postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.AuditEvents(receipt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	observations := 0
	for _, event := range events {
		if event.Action != "grant.result" {
			continue
		}
		raw, err := json.Marshal(event.Details["signed_report"])
		if err != nil {
			t.Fatal(err)
		}
		var report protocol.ResultReport
		if err := json.Unmarshal(raw, &report); err != nil {
			t.Fatal(err)
		}
		request, err := reopened.GetGrantDispatch(context.Background(), report.Result.GrantID)
		if err != nil {
			t.Fatal(err)
		}
		id := resultreport.Identity{RunnerID: "runner-1", TenantID: "tenant-1", RunnerGroup: "payments-prod"}
		verifier := resultreport.Verifier{Keys: resultreport.ScopedKeys{id: observationResultKey{receipt.Key}}}
		if err := verifier.Verify(context.Background(), id, request.Grant, report); err != nil {
			t.Fatal(err)
		}
		if report.Result.Runtime != nil {
			observations++
			hash, err := adapter.RuntimeObservationHash(*request.Result.Runtime)
			if err != nil || hash != report.Result.Runtime.Hash {
				t.Fatal("runtime body lost on reopen")
			}
			if protocol.ValidateObservationResult(request.Grant, request.Result) != nil || request.Result.Health.Query == "" || request.Result.Health.SampledAt == "" {
				t.Fatal("observation proof lost on reopen")
			}
		}
	}
	if observations != 2 {
		t.Fatalf("persisted observations=%d", observations)
	}
}
