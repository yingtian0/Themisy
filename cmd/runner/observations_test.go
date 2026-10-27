package main

import (
	"context"
	"fmt"
	"testing"
	"themisy/adapters/datadog"
	ecsadapter "themisy/adapters/ecs"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
)

func TestECSObservationCompositionRequiresReadRoleHealthAndCapability(t *testing.T) {
	for _, kind := range []string{"valid", "capability", "role", "query", "credential", "write-role"} {
		t.Run(kind, func(t *testing.T) {
			c := settings{Mode: "production", TenantID: "tenant-1", Capabilities: []protocol.Capability{protocol.CapabilityDeploy, protocol.CapabilityVerify}, Credentials: credentialSettings{DatadogCredentialFile: writeSecret(t, t.TempDir(), "health.json", `{"api_key":"private-fixture-api","app_key":"private-fixture-app","expires_at":"2099-01-01T00:00:00Z"}`)}, Adapters: adapterSettings{Datadog: datadogSettings{Site: datadog.SiteUS1, Queries: []datadogQuerySettings{{Service: "payment-api", Environment: "staging", Expression: "metric.error", Comparator: "lte", Threshold: 1, Aggregation: "last", MinimumPoints: 1, MaximumAge: "1m"}}}, ECS: ecsSettings{Targets: []ecsTargetSettings{{Service: "payment-api", Environment: "staging", Region: "ap-northeast-1", ClusterARN: "arn:aws:ecs:ap-northeast-1:111122223333:cluster/staging", ServiceARN: "arn:aws:ecs:ap-northeast-1:111122223333:service/staging/payment-api", DeployRoleARN: "arn:aws:iam::111122223333:role/deploy", VerifyRoleARN: "arn:aws:iam::111122223333:role/read", TaskDefinitions: []ecsTaskDefinitionSettings{{ArtifactDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TaskDefinitionARN: "arn:aws:ecs:ap-northeast-1:111122223333:task-definition/payment-api:2", ExpectedTaskDefinitions: []string{"arn:aws:ecs:ap-northeast-1:111122223333:task-definition/payment-api:1"}, ExpectedContainerDigests: map[string]string{"app": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}}}}}
			targets, roles, err := buildECSTargets(c)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := ecsadapter.New(ecsadapter.Config{Targets: targets}, func(context.Context, string, credentials.Credential) (ecsadapter.API, error) {
				return nil, fmt.Errorf("must not call provider during composition")
			})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "capability":
				c.Capabilities = nil
			case "role":
				c.Adapters.ECS.Targets[0].VerifyRoleARN = ""
			case "query":
				c.Adapters.Datadog.Queries = nil
			case "credential":
				c.Credentials.DatadogCredentialFile = ""
			case "write-role":
				c.Adapters.ECS.Targets[0].VerifyRoleARN = c.Adapters.ECS.Targets[0].DeployRoleARN
				_, _, err := buildECSTargets(c)
				if err == nil {
					t.Fatal("read role shares write role")
				}
				return
			}
			observer, err := buildObservations(context.Background(), c, instance, &credentials.AWSBroker{Roles: roles})
			if kind == "valid" {
				if err != nil || observer == nil || observer.Runtime == nil || observer.Health == nil {
					t.Fatalf("observer=%+v err=%v", observer, err)
				}
				if roles[credentials.AWSRoleKey{TenantID: c.TenantID, Service: "payment-api", Environment: "staging", Purpose: credentials.PurposeVerify}].RoleARN == "" {
					t.Fatal("read role missing")
				}
			} else if err == nil {
				t.Fatal("incomplete verification configuration accepted")
			}
		})
	}
}
