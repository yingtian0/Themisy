package workflow

import (
	"themisy/internal/domain"
	"themisy/internal/executor"
	"themisy/internal/store"
	"time"
)

func compensationRefusal(run domain.ReleaseRun, service string, source executor.Deployment, now time.Time) *domain.AuthorizationStop {
	if run.Plan.CreatedAt.IsZero() {
		return nil
	} // legacy histories/fixtures gain no new authority
	stop := &domain.AuthorizationStop{ReasonCode: "COMPENSATION_UNAVAILABLE", Object: "compensation", Service: service, ObservedAt: now.UTC()}
	for _, phase := range run.Plan.Phases {
		for _, planned := range phase.Steps {
			if planned.Service == service {
				duration, err := time.ParseDuration(planned.RollbackDeadline)
				if err != nil || duration <= 0 || source.StartedAt.IsZero() {
					return stop
				}
				stop.Deadline = source.StartedAt.Add(duration)
				if !now.Before(stop.Deadline) {
					stop.ReasonCode = "COMPENSATION_EXPIRED"
					return stop
				}
				if planned.RollbackTargetHash == "" || source.GrantID == "" {
					return stop
				}
				for _, step := range run.Steps {
					if step.Service == service && step.Execution != nil && step.Execution.Status == string(store.ExecutionSucceeded) && step.Execution.ExternalExecutionID == source.ExternalID {
						return nil
					}
				}
				return stop
			}
		}
	}
	return stop
}
