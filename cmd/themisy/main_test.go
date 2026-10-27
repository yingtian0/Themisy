package main

import (
	"context"
	"strings"
	"testing"

	"themisy/internal/application"
	"themisy/internal/executor"
)

func TestRuntimeGrantExecutorHasNoSyntheticVerification(t *testing.T) {
	grants := &application.Grants{}
	runtime := newGrantExecutor(grants)
	if runtime.Grants != grants || runtime.Verification != nil {
		t.Fatal("runtime must preserve Grant execution without supplying a mock verifier")
	}
	if _, err := runtime.Verify(context.Background(), executor.Deployment{ExternalID: "accepted-provider-execution"}); err == nil || !strings.Contains(err.Error(), "verified deployment observation unavailable") {
		t.Fatalf("accepted dispatch acquired synthetic evidence: %v", err)
	}
}
