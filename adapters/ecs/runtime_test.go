package ecs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

func TestRuntimeGateFailsClosedAcrossDeployRollbackAndReconcile(t *testing.T) {
	cases := []struct {
		name   string
		change func(*fakeECS)
	}{
		{"inactive", func(f *fakeECS) { f.status = "DRAINING" }},
		{"zero desired", func(f *fakeECS) { f.desiredCount = 0; f.runningCount = 0 }},
		{"under running", func(f *fakeECS) { f.runningCount = 0 }},
		{"pending", func(f *fakeECS) { f.pendingCount = 1 }},
		{"rollout pending", func(f *fakeECS) { f.deploymentState = "IN_PROGRESS" }},
		{"rollout failed", func(f *fakeECS) { f.deploymentState = "FAILED" }},
		{"foreign service", func(f *fakeECS) {
			f.serviceHook = func(o *awsecs.DescribeServicesOutput) {
				if f.describeCalls > 1 {
					o.Services[0].ServiceArn = aws.String("foreign")
				}
			}
		}},
		{"foreign cluster", func(f *fakeECS) {
			f.serviceHook = func(o *awsecs.DescribeServicesOutput) {
				if f.describeCalls > 1 {
					o.Services[0].ClusterArn = aws.String("foreign")
				}
			}
		}},
		{"missing primary", func(f *fakeECS) {
			f.serviceHook = func(o *awsecs.DescribeServicesOutput) { o.Services[0].Deployments = nil }
		}},
		{"extra deployment", func(f *fakeECS) {
			f.serviceHook = func(o *awsecs.DescribeServicesOutput) {
				o.Services[0].Deployments = append(o.Services[0].Deployments, o.Services[0].Deployments[0])
			}
		}},
		{"wrong primary", func(f *fakeECS) {
			f.serviceHook = func(o *awsecs.DescribeServicesOutput) {
				o.Services[0].Deployments[0].TaskDefinition = aws.String("foreign")
			}
		}},
		{"primary pending", func(f *fakeECS) {
			f.serviceHook = func(o *awsecs.DescribeServicesOutput) { o.Services[0].Deployments[0].PendingCount = 1 }
		}},
		{"missing list", func(f *fakeECS) {
			f.listHook = func(*awsecs.ListTasksInput) (*awsecs.ListTasksOutput, error) { return nil, nil }
		}},
		{"list error", func(f *fakeECS) {
			f.listHook = func(*awsecs.ListTasksInput) (*awsecs.ListTasksOutput, error) { return nil, context.DeadlineExceeded }
		}},
		{"empty list", func(f *fakeECS) {
			f.listHook = func(*awsecs.ListTasksInput) (*awsecs.ListTasksOutput, error) { return &awsecs.ListTasksOutput{}, nil }
		}},
		{"duplicate list", func(f *fakeECS) {
			f.desiredCount = 2
			f.runningCount = 2
			f.listHook = func(*awsecs.ListTasksInput) (*awsecs.ListTasksOutput, error) {
				return &awsecs.ListTasksOutput{TaskArns: []string{"task", "task"}}, nil
			}
		}},
		{"missing task", func(f *fakeECS) { f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks = nil } }},
		{"task failure", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Failures = []types.Failure{{Reason: aws.String("missing")}} }
		}},
		{"foreign task", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].TaskArn = aws.String("foreign") }
		}},
		{"foreign task cluster", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].ClusterArn = aws.String("foreign") }
		}},
		{"foreign service task", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].Group = aws.String("service:other") }
		}},
		{"old task definition", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].TaskDefinitionArn = aws.String("old") }
		}},
		{"pending task", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].LastStatus = aws.String("PENDING") }
		}},
		{"stopping task", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].DesiredStatus = aws.String("STOPPED") }
		}},
		{"unknown task health", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].HealthStatus = types.HealthStatusUnknown }
		}},
		{"unhealthy container", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) {
				o.Tasks[0].Containers[0].HealthStatus = types.HealthStatusUnhealthy
			}
		}},
		{"container stopped", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].Containers[0].LastStatus = aws.String("STOPPED") }
		}},
		{"wrong digest", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) {
				o.Tasks[0].Containers[0].ImageDigest = aws.String("sha256:" + strings.Repeat("b", 64))
			}
		}},
		{"missing digest", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].Containers[0].ImageDigest = nil }
		}},
		{"foreign container task", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].Containers[0].TaskArn = aws.String("foreign") }
		}},
		{"unexpected sidecar", func(f *fakeECS) {
			f.taskHook = func(o *awsecs.DescribeTasksOutput) {
				o.Tasks[0].Containers = append(o.Tasks[0].Containers, types.Container{Name: aws.String("sidecar")})
			}
		}},
		{"missing container", func(f *fakeECS) { f.taskHook = func(o *awsecs.DescribeTasksOutput) { o.Tasks[0].Containers = nil } }},
	}
	for _, tc := range cases {
		for _, mode := range []string{"deploy already selected", "deploy accepted", "rollback already selected", "rollback accepted", "reconcile deploy", "reconcile rollback"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				f := newFixture(t)
				tc.change(f.client)
				rollback := strings.Contains(mode, "rollback")
				desired := f.definition.ARN
				if rollback {
					desired = f.target.RollbackTaskDefinition
				}
				f.client.current = desired
				if strings.Contains(mode, "accepted") {
					if rollback {
						f.client.current = f.definition.ARN
					} else {
						f.client.current = f.target.RollbackTaskDefinition
					}
				}
				var err error
				if strings.HasPrefix(mode, "reconcile") {
					op := adapter.ReconcileDeploy
					if rollback {
						op = adapter.ReconcileRollback
					}
					result, e := f.adapter.Reconcile(context.Background(), adapter.ReconcileRequest{Operation: op, Target: f.request.Target, ArtifactDigest: f.request.ArtifactDigest, IdempotencyKey: "key", OriginalDeployment: adapter.Deployment{ExternalExecutionID: "original"}}, credentials.Credential{})
					err = e
					if result.Status == adapter.ReconcileSucceeded {
						t.Fatal("runtime incorrectly succeeded")
					}
				} else if rollback {
					_, err = f.adapter.Rollback(context.Background(), adapter.RollbackRequest{Target: f.request.Target, IdempotencyKey: "key", OriginalDeployment: adapter.Deployment{ExternalExecutionID: "original"}}, credentials.Credential{})
				} else {
					_, err = f.adapter.Deploy(context.Background(), f.request, credentials.Credential{})
				}
				if !adapter.IsClass(err, adapter.ErrorUnknown) {
					t.Fatalf("err=%v", err)
				}
				want := 0
				if strings.Contains(mode, "accepted") {
					want = 1
				}
				if f.client.updateCalls != want {
					t.Fatalf("writes=%d want=%d", f.client.updateCalls, want)
				}
			})
		}
	}
}

func TestRuntimePaginationBoundsAndChunks(t *testing.T) {
	for _, mode := range []string{"success", "repeated token", "page bound", "duplicate response", "over bound"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.client.current = f.definition.ARN
			f.client.desiredCount = 101
			f.client.runningCount = 101
			if mode == "over bound" {
				f.client.desiredCount = 1001
				f.client.runningCount = 1001
			}
			f.client.listHook = func(input *awsecs.ListTasksInput) (*awsecs.ListTasksOutput, error) {
				if aws.ToString(input.ServiceName) != "payment" || input.DesiredStatus != types.DesiredStatusRunning || aws.ToInt32(input.MaxResults) != 100 {
					t.Fatalf("bad list input: %#v", input)
				}
				if mode == "repeated token" {
					return &awsecs.ListTasksOutput{NextToken: aws.String("same")}, nil
				}
				if mode == "page bound" {
					return &awsecs.ListTasksOutput{NextToken: aws.String(fmt.Sprint(f.client.listCalls))}, nil
				}
				start, end := 0, 100
				if input.NextToken != nil {
					start, end = 100, 101
				}
				o := &awsecs.ListTasksOutput{}
				for i := start; i < end; i++ {
					o.TaskArns = append(o.TaskArns, fmt.Sprintf("task-%d", i))
				}
				if start == 0 {
					o.NextToken = aws.String("next")
				}
				return o, nil
			}
			f.client.taskHook = func(o *awsecs.DescribeTasksOutput) {
				template := o.Tasks[0]
				start, end := 0, 100
				if f.client.taskCalls == 2 {
					start, end = 100, 101
				}
				o.Tasks = nil
				for i := start; i < end; i++ {
					task := template
					task.TaskArn = aws.String(fmt.Sprintf("task-%d", i))
					task.Containers = append([]types.Container(nil), template.Containers...)
					task.Containers[0].TaskArn = task.TaskArn
					o.Tasks = append(o.Tasks, task)
				}
				if mode == "duplicate response" {
					o.Tasks[1] = o.Tasks[0]
				}
			}
			_, err := f.adapter.Deploy(context.Background(), f.request, credentials.Credential{})
			if mode == "success" {
				if err != nil || f.client.listCalls != 2 || f.client.taskCalls != 2 {
					t.Fatalf("err=%v list=%d describe=%d", err, f.client.listCalls, f.client.taskCalls)
				}
			} else if !adapter.IsClass(err, adapter.ErrorUnknown) {
				t.Fatalf("err=%v", err)
			}
			if f.client.listCalls > maxTaskPages || f.client.updateCalls != 0 {
				t.Fatalf("unbounded reads or write: %#v", f.client)
			}
		})
	}
}

func TestRuntimeConfigurationRequiresEveryContainer(t *testing.T) {
	for _, mode := range []string{"deploy missing", "deploy invalid", "rollback missing", "rollback invalid"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			target := f.target
			if strings.HasPrefix(mode, "deploy") {
				d := f.definition
				d.ExpectedContainerDigests = nil
				if strings.HasSuffix(mode, "invalid") {
					d.ExpectedContainerDigests = map[string]string{"app": "latest"}
				}
				target.TaskDefinitions = map[string]TaskDefinition{f.request.ArtifactDigest: d}
			} else {
				target.RollbackContainerDigests = nil
				if strings.HasSuffix(mode, "invalid") {
					target.RollbackContainerDigests = map[string]string{"": "sha256:" + strings.Repeat("a", 64)}
				}
			}
			if err := (Config{Targets: map[TargetKey]Target{{Service: "payment", Environment: "prod"}: target}}).Validate(); err == nil {
				t.Fatal("invalid expectations accepted")
			}
		})
	}
}

func TestUnknownRuntimeRecoversReadOnly(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprint(rollback), func(t *testing.T) {
			f := newFixture(t)
			f.client.deploymentState = "IN_PROGRESS"
			op := adapter.ReconcileDeploy
			original := adapter.Deployment{ExternalExecutionID: "original"}
			if rollback {
				op = adapter.ReconcileRollback
				f.client.current = f.definition.ARN
			}
			var err error
			if rollback {
				_, err = f.adapter.Rollback(context.Background(), adapter.RollbackRequest{Target: f.request.Target, IdempotencyKey: "key", OriginalDeployment: original}, credentials.Credential{})
			} else {
				_, err = f.adapter.Deploy(context.Background(), f.request, credentials.Credential{})
			}
			if !adapter.IsClass(err, adapter.ErrorUnknown) || f.client.updateCalls != 1 {
				t.Fatalf("err=%v writes=%d", err, f.client.updateCalls)
			}
			request := adapter.ReconcileRequest{Operation: op, Target: f.request.Target, IdempotencyKey: "key", OriginalDeployment: original, ArtifactDigest: f.request.ArtifactDigest}
			result, err := f.adapter.Reconcile(context.Background(), request, credentials.Credential{})
			if !adapter.IsClass(err, adapter.ErrorUnknown) || result.Status != adapter.ReconcilePending || f.client.updateCalls != 1 {
				t.Fatalf("result=%#v err=%v writes=%d", result, err, f.client.updateCalls)
			}
			f.client.deploymentState = "COMPLETED"
			result, err = f.adapter.Reconcile(context.Background(), request, credentials.Credential{})
			if err != nil || result.Status != adapter.ReconcileSucceeded || f.client.updateCalls != 1 {
				t.Fatalf("result=%#v err=%v writes=%d", result, err, f.client.updateCalls)
			}
		})
	}
}

func TestConfiguredSidecarAndDuplicateContainerNames(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			f := newFixture(t)
			f.client.current = f.definition.ARN
			f.definition.ExpectedContainerDigests["sidecar"] = "sha256:" + strings.Repeat("b", 64)
			// Apply the changed operator configuration by constructing a new adapter.
			configured, err := New(Config{Targets: map[TargetKey]Target{{Service: f.request.Target.Service, Environment: f.request.Target.Environment}: f.target}}, func(context.Context, string, credentials.Credential) (API, error) { return f.client, nil })
			if err != nil {
				t.Fatal(err)
			}
			f.adapter = configured
			f.client.taskHook = func(o *awsecs.DescribeTasksOutput) {
				c := o.Tasks[0].Containers[0]
				c.Name = aws.String("sidecar")
				c.ImageDigest = aws.String("sha256:" + strings.Repeat("b", 64))
				if duplicate {
					c.Name = aws.String("app")
				}
				o.Tasks[0].Containers = append(o.Tasks[0].Containers, c)
			}
			_, err = f.adapter.Deploy(context.Background(), f.request, credentials.Credential{})
			if duplicate {
				if !adapter.IsClass(err, adapter.ErrorUnknown) {
					t.Fatalf("err=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
