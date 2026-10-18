//go:build integration

package workflow

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"themisy/internal/policy"
	postgresstore "themisy/internal/store/postgres"
)

func TestPostgresDeploymentSuccessResponseLoss(t *testing.T) {
	url := os.Getenv("THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL")
	if url == "" && os.Getenv("THEMISY_INTEGRATION") == "1" {
		url = os.Getenv("THEMISY_DATABASE_URL")
		if url == "" {
			url = "postgres://themisy:themisy@localhost:5432/themisy?sslmode=disable"
		}
	}
	if url == "" {
		t.Skip("requires dedicated disposable THEMISY_DEPLOYMENT_RESULT_TEST_DATABASE_URL")
	}
	if err := postgresstore.Migrate(url, false); err != nil {
		t.Fatal(err)
	}
	st, err := postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	run, _, exec := workflowFixture(t, false, nil)
	run.ID += "-" + time.Now().UTC().Format("20060102150405.000000000")
	run.RequestID, run.WorkflowID = run.ID, run.ID
	if _, _, err := st.CreateRun(&run); err != nil {
		t.Fatal(err)
	}
	input := DeployInput{RunID: run.ID, Service: "identity", Environment: "staging", DesiredVersion: "sha-1", IdempotencyKey: run.ID + "/pg-response-loss"}
	counted := &countRollbackExecutor{ReleaseExecutor: exec}
	a := NewActivities(lostSuccessResponseStore{st}, policy.New(), counted)
	if _, err := a.Deploy(context.Background(), input); err == nil {
		t.Fatal("expected lost success response")
	}
	// Drop all DB connections and reassemble Activities as a new worker would.
	st.Close()
	st, err = postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	noWrites := &forbidWriteExecutor{ReleaseExecutor: exec}
	restarted := NewActivities(st, policy.New(), noWrites)
	deploy, err := restarted.Deploy(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if deploy.Deployment.RunID != run.ID || deploy.Deployment.Service != "identity" || deploy.Deployment.Environment != "staging" || deploy.Deployment.DesiredVersion != "sha-1" || deploy.Deployment.IdempotencyKey != input.IdempotencyKey {
		t.Fatalf("incomplete result: %+v", deploy)
	}
	if _, err := restarted.Verify(context.Background(), VerifyInput{Deployment: deploy.Deployment}); err != nil {
		t.Fatal(err)
	}
	a = NewActivities(lostSuccessResponseStore{st}, policy.New(), counted)
	rollbackInput := RollbackInput{Deploy: input, Deployment: deploy.Deployment}
	if _, err := a.Rollback(context.Background(), rollbackInput); err == nil {
		t.Fatal("expected lost rollback response")
	}
	st.Close()
	st, err = postgresstore.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	restarted = NewActivities(st, policy.New(), noWrites)
	rollback, err := restarted.Rollback(context.Background(), rollbackInput)
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.Rollback(context.Background(), rollbackInput)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rollback, again) {
		t.Fatal("rollback result changed after reload")
	}
	if _, err := restarted.Verify(context.Background(), VerifyInput{Deployment: rollback.Deployment}); err != nil {
		t.Fatal(err)
	}
	if noWrites.calls != 0 || exec.DeployCalls(input.IdempotencyKey) != 1 || counted.rollbacks != 1 {
		t.Fatal("duplicate write")
	}
}
