package ecs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"time"
)

func TestRuntimeObservationsAreReadOnlyAndNeverPromotePending(t *testing.T) {
	for _, operation := range []adapter.ReconcileOperation{adapter.ReconcileDeploy, adapter.ReconcileRollback} {
		for _, mode := range []string{"healthy", "pending", "definition-error", "wrong-target", "cancelled"} {
			t.Run(string(operation)+"/"+mode, func(t *testing.T) {
				f := newFixture(t)
				f.client.current = f.definition.ARN
				r := adapter.ReconcileRequest{Target: f.request.Target, ArtifactDigest: f.request.ArtifactDigest, IdempotencyKey: "observation", Operation: operation, OriginalDeployment: adapter.Deployment{ExternalExecutionID: "prior"}}
				if operation == adapter.ReconcileRollback {
					f.client.current = f.target.RollbackTaskDefinition
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch mode {
				case "pending":
					f.client.deploymentState = "IN_PROGRESS"
				case "definition-error":
					f.client.definitionErr = errors.New("raw-secret-canary")
				case "wrong-target":
					r.Target.Service = "unconfigured"
				case "cancelled":
					cancel()
				}
				observation, err := f.adapter.ReadRuntime(ctx, r, credentials.Credential{})
				if f.client.updateCalls != 0 {
					t.Fatal("runtime read wrote provider state")
				}
				if mode == "wrong-target" || mode == "cancelled" {
					if err == nil || observation.Hash != "" {
						t.Fatal("invalid request observed")
					}
					return
				}
				if err != nil || observation.Hash == "" {
					t.Fatal(err)
				}
				hash, _ := RuntimeObservationHash(observation)
				if hash != observation.Hash {
					t.Fatal("observation hash")
				}
				raw, _ := json.Marshal(observation)
				if strings.Contains(string(raw), "raw-secret-canary") || strings.Contains(string(raw), "arn:aws:") {
					t.Fatal("raw provider detail exported")
				}
				if mode == "healthy" {
					if observation.Status != adapter.VerificationPass || observation.Summary.TaskCount != 1 || observation.Summary.RolloutState != "COMPLETED" {
						t.Fatal("confirmed runtime lost")
					}
				} else if observation.Status != adapter.VerificationInconclusive || observation.Summary.TaskCount != 0 {
					t.Fatal("unconfirmed runtime promoted")
				}
			})
		}
	}
}
func TestRuntimeObservationClockReversalRefused(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.adapter.now = func() time.Time {
		calls++
		if calls == 1 {
			return time.Unix(100, 0)
		}
		return time.Unix(99, 0)
	}
	r := adapter.ReconcileRequest{Target: f.request.Target, ArtifactDigest: f.request.ArtifactDigest, IdempotencyKey: "read"}
	if o, e := f.adapter.ReadRuntime(context.Background(), r, credentials.Credential{}); e == nil || o.Hash != "" {
		t.Fatal("clock reversal accepted")
	}
}
