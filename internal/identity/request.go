package identity

import (
	"context"
	"slices"
	"time"
)

// RequestActor is populated only by the verified HTTP boundary, never JSON.
type RequestActor struct {
	TenantID, Subject, Issuer, SubjectType string
	Roles                                  []string
	ProofToken                             string `json:"-"`
	ExpiresAt                              time.Time
}
type actorContextKey struct{}
type actorContext struct {
	actor RequestActor
	now   func() time.Time
}

func WithRequestActor(ctx context.Context, a RequestActor, now func() time.Time) context.Context {
	a.Roles = slices.Clone(a.Roles)
	if now == nil {
		now = time.Now
	}
	return context.WithValue(ctx, actorContextKey{}, actorContext{a, now})
}
func RequestActorFromContext(ctx context.Context) (RequestActor, bool) {
	value, ok := ctx.Value(actorContextKey{}).(actorContext)
	if !ok || ctx.Err() != nil || value.actor.ProofToken == "" || !value.actor.ExpiresAt.After(value.now().UTC()) {
		return RequestActor{}, false
	}
	a := value.actor
	a.Roles = slices.Clone(a.Roles)
	return a, true
}

func HasRequestActor(ctx context.Context) bool {
	_, ok := ctx.Value(actorContextKey{}).(actorContext)
	return ok
}
