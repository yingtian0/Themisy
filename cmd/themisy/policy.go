package main

import (
	"context"
	"themisy/internal/config"
	"themisy/internal/grant"
	"themisy/internal/policy"
	postgresstore "themisy/internal/store/postgres"
)

func buildPolicyDistribution(ctx context.Context, c config.Config, st *postgresstore.Store) (*policy.Distributor, error) {
	if c.Policy.Mode != "distributed" {
		return nil, nil
	}
	trust := grant.FileKeys{Path: c.Policy.TrustBundleFile, Issuer: c.Policy.Issuer, Tenant: c.Identity.TenantID}
	if err := trust.Validate(ctx); err != nil {
		return nil, err
	}
	groups := map[string]bool{}
	for _, group := range c.Policy.RunnerGroups {
		groups[group] = true
	}
	return &policy.Distributor{TenantID: c.Identity.TenantID, Issuer: c.Policy.Issuer, Keys: policy.FileBundleKeys{Trust: trust}, Store: st, AllowedGroups: groups}, nil
}
