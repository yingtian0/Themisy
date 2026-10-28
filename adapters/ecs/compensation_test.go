package ecs

import (
	"testing"
	"themisy/pkg/adapter"
)

func TestCompensationTargetHashPinsCustomerPredecessor(t *testing.T) {
	f := newFixture(t)
	key := adapter.Target{Service: f.request.Target.Service, Environment: f.request.Target.Environment}
	original, err := f.adapter.CompensationTargetHash(key)
	if err != nil {
		t.Fatal(err)
	}
	target := f.target
	target.RollbackTaskDefinition += "-changed"
	changed, err := HashCompensationTarget(key, target)
	if err != nil || changed == original {
		t.Fatal("predecessor change did not invalidate approval")
	}
	target = f.target
	target.RollbackContainerDigests = map[string]string{}
	if _, err := HashCompensationTarget(key, target); err == nil {
		t.Fatal("unknown predecessor containers accepted")
	}
}
