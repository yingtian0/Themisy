package policy

import (
	"context"

	"themisy/internal/domain"
)

const Version = "builtin-v1"

type builtinEvaluator struct{}

func (builtinEvaluator) Evaluate(_ context.Context, input Input) (Evaluation, error) {
	reasons := []string{}
	decision := domain.DecisionAllow
	requiredRoles := []string(nil)
	switch {
	case input.UserID == "" || input.AgentID == "":
		decision, reasons = domain.DecisionDeny, []string{"MISSING_SUBJECT"}
	case !containsString(input.AgentScopes, "release:deploy"):
		decision, reasons = domain.DecisionDeny, []string{"MISSING_DELEGATED_SCOPE"}
	case input.Environment == domain.EnvironmentProduction && !containsString(input.AgentScopes, "release:production"):
		decision, reasons = domain.DecisionDeny, []string{"MISSING_PRODUCTION_SCOPE"}
	case !input.CISuccess:
		decision, reasons = domain.DecisionDeny, []string{"CI_NOT_SUCCESSFUL"}
	case !input.DependenciesHealthy:
		decision, reasons = domain.DecisionDeny, []string{"DEPENDENCIES_UNHEALTHY"}
	case input.DestructiveMigration:
		decision, reasons, requiredRoles = domain.DecisionRequireApproval, []string{"DESTRUCTIVE_DB_MIGRATION"}, []string{"service-owner", "sre"}
	case input.Risk == "high" || input.Risk == "critical":
		decision, reasons, requiredRoles = domain.DecisionRequireApproval, []string{"HIGH_BLAST_RADIUS"}, []string{"service-owner"}
	default:
		reasons = []string{"POLICY_ALLOW"}
	}
	hash, err := InputHash(input)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Decision: decision, Reasons: reasons, RequiredRoles: requiredRoles, InputHash: hash, PolicyVersion: Version}, nil
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
