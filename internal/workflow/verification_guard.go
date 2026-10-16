package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"

	"go.temporal.io/sdk/temporal"
	"themisy/internal/domain"
)

func verificationGuardError(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), "PRODUCTION_VERIFICATION_REQUIRED", err)
}

// Validate the entire mapping before the first dispatch, including later phases.
// This is an Activity boundary guard: changing it does not change Workflow commands.
func validateProductionVerification(run domain.ReleaseRun) error {
	production := run.Environment == domain.EnvironmentProduction || run.Plan.Environment == domain.EnvironmentProduction
	for _, phase := range run.Plan.Phases {
		for _, step := range phase.Steps {
			production = production || step.Scheduling.Environment == domain.EnvironmentProduction
		}
	}
	if !production {
		return nil
	}
	if run.Environment != domain.EnvironmentProduction || run.Plan.Environment != domain.EnvironmentProduction {
		return fmt.Errorf("production run and plan environments must agree")
	}
	planned := make(map[string]domain.PlanStep)
	for _, phase := range run.Plan.Phases {
		for _, step := range phase.Steps {
			if _, duplicate := planned[step.Service]; duplicate {
				return fmt.Errorf("duplicate production plan service %q", step.Service)
			}
			if step.Service == "" || !step.VerificationRequired || step.Phase != phase.Number {
				return fmt.Errorf("production plan step %q must require verification and match its phase", step.Service)
			}
			if step.Scheduling.Environment != "" && step.Scheduling.Environment != domain.EnvironmentProduction {
				return fmt.Errorf("production scheduling environment differs for %q", step.Service)
			}
			planned[step.Service] = step
		}
	}
	if len(planned) == 0 || len(planned) != len(run.Steps) {
		return fmt.Errorf("production run must match every plan step")
	}
	seen := make(map[string]bool)
	for _, step := range run.Steps {
		plan, exists := planned[step.Service]
		if !exists || seen[step.Service] || !step.VerificationRequired || step.Phase != plan.Phase || step.Change.Service != step.Service || step.Change.DesiredVersion != plan.DesiredVersion {
			return fmt.Errorf("production run step %q must match a unique verified plan step", step.Service)
		}
		seen[step.Service] = true
	}
	return nil
}

// Compare the wire representation, not time.Location pointers or omitted empty
// fields changed by persistence/Temporal serialization. UTC preserves each instant.
func equivalentPlans(left, right domain.ReleasePlan) (bool, error) {
	left.CreatedAt, left.ExpiresAt = left.CreatedAt.UTC(), left.ExpiresAt.UTC()
	right.CreatedAt, right.ExpiresAt = right.CreatedAt.UTC(), right.ExpiresAt.UTC()
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftJSON, rightJSON), nil
}
