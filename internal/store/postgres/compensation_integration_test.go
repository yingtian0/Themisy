//go:build integration

package postgres

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/store"
	"time"
)

func TestCompensationConsumptionSurvivesRestart(t *testing.T) {
	url := os.Getenv("THEMISY_IDENTITY_DIRECTORY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires owned compensation fixture")
	}
	ctx := context.Background()
	if err := MigrateRunner(ctx, url); err != nil {
		t.Fatal(err)
	}
	open := func() *Store {
		t.Helper()
		s, err := NewRunnerJournal(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first, second := open(), open()
	defer second.Close()
	now := time.Now().UTC()
	tenant := fmt.Sprintf("compensation-%d", now.UnixNano())
	record := store.RunnerActionRecord{GrantID: "rollback", RunID: "run", StepID: "step", TenantID: tenant, RunnerGroup: "runner", Nonce: "nonce", IdempotencyKey: "key", RequestHash: "request", CompensationSourceNonce: "source", CreatedAt: now, UpdatedAt: now}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := record
			r.Nonce = fmt.Sprint(i)
			r.IdempotencyKey = fmt.Sprint(i)
			r.GrantID = fmt.Sprint(i)
			st := first
			if i == 1 {
				st = second
			}
			_, created, err := st.ReserveRunnerAction(ctx, r, domain.AuditEvent{ID: tenant + fmt.Sprint(i), CorrelationID: tenant, Timestamp: now})
			if err == nil && created {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("concurrent compensation admissions=%d", admitted.Load())
	}
	first.Close()
	first = open()
	defer first.Close()
	record.Nonce = "after-restart"
	record.IdempotencyKey = "new-key"
	if _, created, err := first.ReserveRunnerAction(ctx, record, domain.AuditEvent{ID: tenant + "restart", Timestamp: now}); err == nil || created {
		t.Fatal("restart reset compensation consumption")
	}
}
