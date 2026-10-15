// Package credentials defines the Runner-local credential acquisition boundary.
// Credentials obtained through this package must never be serialized into an
// Action Grant, workflow input, audit event, or Control Plane response.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type Purpose string

const (
	PurposeDeploy   Purpose = "deploy"
	PurposeRollback Purpose = "rollback"
	PurposeVerify   Purpose = "verify"
)

type Request struct {
	Provider    string
	TenantID    string
	RunID       string
	Service     string
	Environment string
	Purpose     Purpose
	GrantID     string
}

func (r Request) Validate() error {
	if r.Provider == "" || r.TenantID == "" || r.Service == "" || r.Environment == "" {
		return errors.New("provider, tenant, service, and environment are required")
	}
	switch r.Purpose {
	case PurposeDeploy, PurposeRollback, PurposeVerify:
		return nil
	default:
		return fmt.Errorf("unsupported credential purpose %q", r.Purpose)
	}
}

// Credential is intentionally opaque to callers outside the Runner. Values
// are copied on construction and acquisition so a caller cannot mutate shared
// broker state.
type Credential struct {
	values    map[string][]byte
	ExpiresAt time.Time
}

// String, GoString, and LogValue keep credential bytes out of accidental
// formatting and structured logs. Callers must explicitly extract a value
// only at the provider boundary that consumes it.
func (Credential) String() string       { return "[REDACTED]" }
func (Credential) GoString() string     { return "credentials.Credential{[REDACTED]}" }
func (Credential) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }
func (Credential) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[REDACTED]"))
}

func New(values map[string][]byte, expiresAt time.Time) Credential {
	return Credential{values: cloneValues(values), ExpiresAt: expiresAt.UTC()}
}

func (c Credential) Value(name string) []byte {
	return append([]byte(nil), c.values[name]...)
}

func (c Credential) ValidAt(now time.Time) bool {
	return len(c.values) > 0 && !c.ExpiresAt.IsZero() && now.UTC().Before(c.ExpiresAt.UTC())
}

func (c Credential) Clone() Credential {
	return New(c.values, c.ExpiresAt)
}

type Broker interface {
	Acquire(context.Context, Request) (Credential, error)
}

func cloneValues(values map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(values))
	for key, value := range values {
		result[key] = append([]byte(nil), value...)
	}
	return result
}
