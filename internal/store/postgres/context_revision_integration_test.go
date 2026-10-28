//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
)

func TestSourceContextFenceSurvivesRestart(t *testing.T) {
	url := os.Getenv("THEMISY_IDENTITY_DIRECTORY_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires owned Context fixture")
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
			if err := st.AcceptContextRevision(ctx, "fence-tenant", "source", 1, "pass-hash"); err != nil {
				t.Fatal(err)
			}
			if err := st.AcceptContextRevision(ctx, "fence-tenant", "source", 2, "negative-hash"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			st = open()
			defer st.Close()
			if st.AcceptContextRevision(ctx, "fence-tenant", "source", 1, "pass-hash") == nil {
				t.Fatal("reopening restored old PASS")
			}
			if st.AcceptContextRevision(ctx, "fence-tenant", "source", 2, "changed-negative") == nil {
				t.Fatal("same revision equivocation accepted")
			}
			if err := st.AcceptContextRevision(ctx, "fence-tenant", "source", 3, "new-pass"); err != nil {
				t.Fatal(err)
			}
			if err := st.AcceptContextRevision(ctx, "other-tenant", "source", 1, "isolated-pass"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
