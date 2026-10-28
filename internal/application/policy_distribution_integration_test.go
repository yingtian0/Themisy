//go:build integration

package application

import (
	"context"
	"os"
	"testing"
	postgresstore "themisy/internal/store/postgres"
)

func TestPostgresPolicyGrantAdmission(t *testing.T) {
	url := os.Getenv("THEMISY_OBSERVATION_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires owned policy fixture")
	}
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	st, err := postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	testPolicyGrantAdmission(t, st)
}
