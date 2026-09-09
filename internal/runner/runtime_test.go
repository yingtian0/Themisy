package runner

import (
	"errors"
	"testing"

	"themisy/pkg/protocol"
)

func TestRuntimeStateBlocksNewWritesWhileDrainingOrFrozen(t *testing.T) {
	state := NewRuntimeState(1)
	if err := state.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := state.Begin(); !errors.Is(err, ErrRunnerAtCapacity) {
		t.Fatalf("second write error=%v", err)
	}
	state.SetMode(protocol.RunnerModeDraining)
	if err := state.Begin(); !errors.Is(err, ErrRunnerDraining) {
		t.Fatalf("draining error=%v", err)
	}
	_, inFlight := state.Snapshot()
	if inFlight != 1 {
		t.Fatalf("drain discarded in-flight work: %d", inFlight)
	}
	state.End()
	state.SetMode(protocol.RunnerModeFrozen)
	if err := state.Begin(); !errors.Is(err, ErrRunnerFrozen) {
		t.Fatalf("frozen error=%v", err)
	}
	state.SetMode(protocol.RunnerModeActive)
	if err := state.Begin(); err != nil {
		t.Fatalf("reactivated error=%v", err)
	}
	state.End()
}

func TestRunnerAdmissionRejectsBeforeCredentialsAndAdapter(t *testing.T) {
	fixture := newRunnerFixture(t)
	state := NewRuntimeState(1)
	state.SetMode(protocol.RunnerModeFrozen)
	fixture.runner.Admission = state
	result, err := fixture.runner.Execute(t.Context(), fixture.grant)
	if err == nil || result.ReasonCode != string(ReasonFrozen) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if fixture.credentials.Calls() != 0 || fixture.adapter.Calls() != 0 {
		t.Fatalf("credentials=%d adapter=%d", fixture.credentials.Calls(), fixture.adapter.Calls())
	}
}
