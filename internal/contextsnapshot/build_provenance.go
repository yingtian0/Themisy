package contextsnapshot

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"themisy/internal/grant"
	"themisy/pkg/protocol"
	"time"
)

const BuildStatementVersion = "themisy.build-provenance/v1"

type BuildStatement struct {
	Service              string             `json:"service"`
	Version              string             `json:"version"`
	Issuer               string             `json:"issuer"`
	TenantID             string             `json:"tenant_id"`
	Repository           string             `json:"repository"`
	CommitSHA            string             `json:"commit_sha"`
	Workflow             string             `json:"workflow"`
	RunID                int64              `json:"run_id"`
	RunAttempt           int                `json:"run_attempt"`
	ArtifactDigest       string             `json:"artifact_digest"`
	DestructiveMigration bool               `json:"destructive_migration"`
	IssuedAt             time.Time          `json:"issued_at"`
	ExpiresAt            time.Time          `json:"expires_at"`
	Signature            protocol.Signature `json:"signature"`
}

func digest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", ErrSourceContext
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func buildMessage(b BuildStatement) ([]byte, error) {
	b.Signature.Value = ""
	b.IssuedAt = b.IssuedAt.UTC()
	b.ExpiresAt = b.ExpiresAt.UTC()
	hash, err := digest(b)
	if err != nil {
		return nil, err
	}
	return []byte(BuildStatementVersion + "\x00" + hash), nil
}
func SignBuildStatement(ctx context.Context, signer grant.Signer, keyID string, b BuildStatement) (BuildStatement, error) {
	if signer == nil || keyID == "" || ctx.Err() != nil {
		return BuildStatement{}, ErrSourceContext
	}
	b.Version = BuildStatementVersion
	b.Signature = protocol.Signature{Algorithm: "Ed25519", KeyID: keyID}
	message, err := buildMessage(b)
	if err != nil {
		return BuildStatement{}, err
	}
	b.Signature, err = signer.Sign(ctx, message)
	if err != nil || b.Signature.Algorithm != "Ed25519" || b.Signature.KeyID != keyID || ctx.Err() != nil {
		return BuildStatement{}, ErrSourceContext
	}
	return b, nil
}
func VerifyBuildStatement(ctx context.Context, b BuildStatement, issuer, tenant string, keys grant.KeyResolver, now time.Time) error {
	if keys == nil || ctx.Err() != nil || b.Version != BuildStatementVersion || b.Issuer != issuer || b.TenantID != tenant || b.Service == "" || b.Repository == "" || !commitSHA.MatchString(b.CommitSHA) || b.Workflow == "" || b.RunID < 1 || b.RunAttempt < 1 || len(b.ArtifactDigest) != 71 || b.ArtifactDigest[:7] != "sha256:" || b.IssuedAt.IsZero() || b.IssuedAt.After(now) || !b.ExpiresAt.After(now) || b.ExpiresAt.Sub(b.IssuedAt) > 30*24*time.Hour || b.Signature.Algorithm != "Ed25519" {
		return ErrSourceContext
	}
	if _, err := hex.DecodeString(b.ArtifactDigest[7:]); err != nil {
		return ErrSourceContext
	}
	key, err := keys.ResolveGrantKey(ctx, issuer, b.Signature.KeyID)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return ErrSourceContext
	}
	message, err := buildMessage(b)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(b.Signature.Value)
	if err != nil || !ed25519.Verify(key, message, signature) || ctx.Err() != nil {
		return ErrSourceContext
	}
	return nil
}
