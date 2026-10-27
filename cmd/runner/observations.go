package main

import (
	"context"
	"fmt"
	"themisy/adapters/datadog"
	ecsadapter "themisy/adapters/ecs"
	runnercore "themisy/internal/runner"
	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
	"themisy/pkg/protocol"
	"time"
)

func buildObservations(ctx context.Context, c settings, deploy adapter.DeployAdapter, broker credentials.Broker) (*runnercore.ObservationVerifier, error) {
	ecs, ok := deploy.(*ecsadapter.Adapter)
	if !ok {
		if c.Mode == "production" {
			return nil, fmt.Errorf("production runtime verification currently requires ECS")
		}
		return nil, nil
	}
	if broker == nil {
		return nil, fmt.Errorf("ECS verification broker unavailable")
	}
	if !runnercore.NewCapabilitySet(c.Capabilities...).Allows(protocol.CapabilityVerify) {
		return nil, fmt.Errorf("ECS execution requires release.verify capability")
	}
	queries := map[datadog.QueryKey]datadog.Query{}
	allowed := map[credentials.DatadogTarget]struct{}{}
	for _, q := range c.Adapters.Datadog.Queries {
		age, err := time.ParseDuration(q.MaximumAge)
		if err != nil {
			return nil, err
		}
		queries[datadog.QueryKey{Service: q.Service, Environment: q.Environment}] = datadog.Query{Expression: q.Expression, Comparator: q.Comparator, Threshold: q.Threshold, Aggregation: q.Aggregation, MinimumPoints: q.MinimumPoints, MaximumAge: age}
	}
	for _, target := range c.Adapters.ECS.Targets {
		if target.VerifyRoleARN == "" {
			return nil, fmt.Errorf("ECS execution requires a separate read-only verify role")
		}
		if _, ok := queries[datadog.QueryKey{Service: target.Service, Environment: target.Environment}]; !ok {
			return nil, fmt.Errorf("ECS target requires a trusted health query")
		}
		allowed[credentials.DatadogTarget{TenantID: c.TenantID, Service: target.Service, Environment: target.Environment}] = struct{}{}
	}
	health, err := datadog.New(datadog.Config{Site: c.Adapters.Datadog.Site, Queries: queries}, nil)
	if err != nil {
		return nil, err
	}
	source := credentials.DatadogCredentialFileSource{Path: c.Credentials.DatadogCredentialFile}
	credential, err := source.Credential(ctx)
	if err != nil || !credential.ValidAt(time.Now()) {
		return nil, fmt.Errorf("ECS execution requires valid Runner-local health credentials")
	}
	return &runnercore.ObservationVerifier{Runtime: ecs, RuntimeBroker: broker, Health: health, HealthBroker: &credentials.DatadogBroker{Source: source, Allowed: allowed}}, nil
}
