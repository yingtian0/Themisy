//go:build integration

package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"themisy/internal/store"
)

func TestIdentityDirectoryFenceSurvivesRestart(t *testing.T) {
	url := os.Getenv("THEMISY_IDENTITY_DIRECTORY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires owned identity Directory fixture")
	}
	ctx := context.Background()
	if err := Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	if err := MigrateRunner(ctx, url); err != nil {
		t.Fatal(err)
	}
	for _, runner := range []bool{false, true} {
		name := "control"
		if runner {
			name = "runner"
		}
		t.Run(name, func(t *testing.T) {
			open := func() *Store {
				t.Helper()
				var s *Store
				var err error
				if runner {
					s, err = NewRunnerJournal(ctx, url)
				} else {
					s, err = New(ctx, url)
				}
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			st := open()
			if err := st.AcceptIdentityDirectory(ctx, "tenant", "directory", 1, "hash-1"); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 24)
			for n := int64(2); n <= 25; n++ {
				wg.Add(1)
				go func(rev int64) {
					defer wg.Done()
					err := st.AcceptIdentityDirectory(ctx, "tenant", "directory", rev, "new-hash")
					if err != nil && err != store.ErrConflict { // conflict can be wrapped
						if !errors.Is(err, store.ErrConflict) {
							errs <- err
						}
					}
				}(n)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}
			st.Close()
			st = open()
			defer st.Close()
			if err := st.AcceptIdentityDirectory(ctx, "tenant", "directory", 25, "new-hash"); err != nil {
				t.Fatal(err)
			}
			if err := st.AcceptIdentityDirectory(ctx, "tenant", "directory", 1, "hash-1"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("rollback accepted: %v", err)
			}
			if err := st.AcceptIdentityDirectory(ctx, "tenant", "directory", 25, "different"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("same revision conflict accepted: %v", err)
			}
			if err := st.AcceptIdentityDirectory(ctx, "other-tenant", "directory", 1, "other"); err != nil {
				t.Fatal(err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := st.AcceptIdentityDirectory(cancelled, "tenant", "directory", 30, "cancelled"); err == nil {
				t.Fatal("cancelled write accepted")
			}
		})
	}
}
