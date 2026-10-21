package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const GitHubProvider = "github-actions"

type GitHubTarget struct {
	TenantID    string
	Service     string
	Environment string
}

// GitHubTokenFileSource reads a short-lived token document written into the
// Runner by a customer-managed workload identity or GitHub App sidecar.
// Expected JSON: {"token":"...","expires_at":"RFC3339"}.
type GitHubTokenFileSource struct {
	Path string
}

func (s GitHubTokenFileSource) Token(_ context.Context) (Credential, error) {
	if strings.TrimSpace(s.Path) == "" {
		return Credential{}, errors.New("github token file is required")
	}
	file, err := os.Open(s.Path)
	if err != nil {
		return Credential{}, fmt.Errorf("open github token file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Credential{}, fmt.Errorf("stat github token file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Credential{}, errors.New("github token file must not be group or world accessible")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var document struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := decoder.Decode(&document); err != nil {
		return Credential{}, fmt.Errorf("decode github token file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Credential{}, errors.New("github token file contains trailing JSON")
	}
	if strings.TrimSpace(document.Token) == "" || document.ExpiresAt.IsZero() {
		return Credential{}, errors.New("github token and expiry are required")
	}
	return New(map[string][]byte{"token": []byte(document.Token)}, document.ExpiresAt), nil
}

type GitHubTokenSource interface {
	Token(context.Context) (Credential, error)
}

type GitHubBroker struct {
	Source  GitHubTokenSource
	Allowed map[GitHubTarget]struct{}
	Now     func() time.Time
}

func (b *GitHubBroker) Acquire(ctx context.Context, request Request) (Credential, error) {
	if err := request.Validate(); err != nil {
		return Credential{}, err
	}
	if request.Provider != GitHubProvider || (request.Purpose != PurposeDeploy && request.Purpose != PurposeRollback && request.Purpose != PurposeVerify) {
		return Credential{}, errors.New("github broker only serves deploy, rollback, and read-only verification")
	}
	target := GitHubTarget{TenantID: request.TenantID, Service: request.Service, Environment: request.Environment}
	if _, ok := b.Allowed[target]; !ok {
		return Credential{}, errors.New("github credential target is not allow-listed")
	}
	if b.Source == nil {
		return Credential{}, errors.New("github token source unavailable")
	}
	credential, err := b.Source.Token(ctx)
	if err != nil {
		return Credential{}, err
	}
	now := time.Now().UTC()
	if b.Now != nil {
		now = b.Now().UTC()
	}
	if len(credential.Value("token")) == 0 || !credential.ValidAt(now.Add(30*time.Second)) {
		return Credential{}, errors.New("github token is missing or too close to expiry")
	}
	return credential.Clone(), nil
}
