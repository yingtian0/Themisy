package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"themisy/internal/grant"
	"themisy/pkg/protocol"
	"time"
)

var ErrDirectory = errors.New("IDENTITY_DIRECTORY_UNAVAILABLE")

const DirectoryVersion = "themisy.identity-directory/v1"

type DirectoryMember struct {
	Issuer  string   `json:"issuer"`
	Subject string   `json:"subject"`
	Enabled bool     `json:"enabled"`
	Roles   []string `json:"roles"`
}
type DirectoryBundle struct {
	Version     string             `json:"version"`
	Issuer      string             `json:"issuer"`
	TenantID    string             `json:"tenant_id"`
	Revision    int64              `json:"revision"`
	IssuedAt    time.Time          `json:"issued_at"`
	ExpiresAt   time.Time          `json:"expires_at"`
	Members     []DirectoryMember  `json:"members"`
	Delegations []Delegation       `json:"delegations"`
	Signature   protocol.Signature `json:"signature"`
}
type DirectoryLedger interface {
	AcceptIdentityDirectory(context.Context, string, string, int64, string) error
}
type FileDirectory struct {
	Path, Issuer, TenantID string
	MinimumRevision        int64
	Keys                   grant.KeyResolver
	Ledger                 DirectoryLedger
	Now                    func() time.Time
}

func directoryPayload(b DirectoryBundle) ([]byte, error) {
	b.Signature.Value = ""
	b.IssuedAt = b.IssuedAt.UTC()
	b.ExpiresAt = b.ExpiresAt.UTC()
	b.Members = slices.Clone(b.Members)
	for i := range b.Members {
		b.Members[i].Roles = slices.Clone(b.Members[i].Roles)
		slices.Sort(b.Members[i].Roles)
	}
	slices.SortFunc(b.Members, func(a, c DirectoryMember) int {
		return strings.Compare(a.Issuer+"\x00"+a.Subject, c.Issuer+"\x00"+c.Subject)
	})
	b.Delegations = slices.Clone(b.Delegations)
	for i := range b.Delegations {
		b.Delegations[i] = copyDelegation(b.Delegations[i])
		slices.Sort(b.Delegations[i].Actions)
		slices.Sort(b.Delegations[i].Environments)
		slices.Sort(b.Delegations[i].ServiceSelectors)
		b.Delegations[i].ExpiresAt = b.Delegations[i].ExpiresAt.UTC()
	}
	slices.SortFunc(b.Delegations, func(a, c Delegation) int { return strings.Compare(a.ID, c.ID) })
	return json.Marshal(b)
}
func directoryMessage(b DirectoryBundle) ([]byte, string, error) {
	raw, err := directoryPayload(b)
	if err != nil {
		return nil, "", ErrDirectory
	}
	sum := sha256.Sum256(raw)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	return []byte(DirectoryVersion + "\x00" + hash), hash, nil
}
func SignDirectory(ctx context.Context, signer grant.Signer, keyID string, b DirectoryBundle) (DirectoryBundle, error) {
	if signer == nil || keyID == "" || ctx.Err() != nil {
		return DirectoryBundle{}, ErrDirectory
	}
	b.Signature = protocol.Signature{Algorithm: "Ed25519", KeyID: keyID}
	message, _, err := directoryMessage(b)
	if err != nil {
		return DirectoryBundle{}, ErrDirectory
	}
	signature, err := signer.Sign(ctx, message)
	if err != nil || signature.KeyID != keyID || signature.Algorithm != "Ed25519" || signature.Value == "" || ctx.Err() != nil {
		return DirectoryBundle{}, ErrDirectory
	}
	b.Signature = signature
	return b, nil
}
func (f *FileDirectory) Read(ctx context.Context) (DirectoryBundle, error) {
	if f == nil || f.Path == "" || f.Issuer == "" || f.TenantID == "" || f.MinimumRevision < 1 || f.Keys == nil || f.Ledger == nil || ctx.Err() != nil {
		return DirectoryBundle{}, ErrDirectory
	}
	info, err := os.Lstat(f.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 1<<20 {
		return DirectoryBundle{}, ErrDirectory
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return DirectoryBundle{}, ErrDirectory
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return DirectoryBundle{}, ErrDirectory
	}
	var b DirectoryBundle
	decoder := json.NewDecoder(io.LimitReader(file, (1<<20)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&b) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return DirectoryBundle{}, ErrDirectory
	}
	now := f.now()
	if b.Version != DirectoryVersion || b.Issuer != f.Issuer || b.TenantID != f.TenantID || b.Revision < f.MinimumRevision || b.IssuedAt.IsZero() || b.IssuedAt.After(now) || !b.ExpiresAt.After(now) || b.ExpiresAt.Sub(b.IssuedAt) > time.Hour || b.Signature.Algorithm != "Ed25519" || b.Signature.KeyID == "" {
		return DirectoryBundle{}, ErrDirectory
	}
	message, hash, err := directoryMessage(b)
	if err != nil {
		return DirectoryBundle{}, ErrDirectory
	}
	key, err := f.Keys.ResolveGrantKey(ctx, b.Issuer, b.Signature.KeyID)
	if err != nil {
		return DirectoryBundle{}, ErrDirectory
	}
	sig, err := base64.RawURLEncoding.DecodeString(b.Signature.Value)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, message, sig) {
		return DirectoryBundle{}, ErrDirectory
	}
	members := map[string]bool{}
	for _, m := range b.Members {
		key := m.Issuer + "\x00" + m.Subject
		if m.Issuer == "" || m.Subject == "" || members[key] {
			return DirectoryBundle{}, ErrDirectory
		}
		members[key] = true
		roles := map[string]bool{}
		for _, role := range m.Roles {
			if strings.TrimSpace(role) != role || role == "" || roles[role] {
				return DirectoryBundle{}, ErrDirectory
			}
			roles[role] = true
		}
	}
	ids := map[string]bool{}
	for _, d := range b.Delegations {
		if d.ID == "" || ids[d.ID] || d.TenantID != b.TenantID || d.Issuer == "" || d.UserSubject == "" || d.AgentID == "" || len(d.Actions) == 0 || len(d.ServiceSelectors) == 0 || len(d.Environments) == 0 || riskLevel(d.MaximumRisk) == 0 || d.ExpiresAt.IsZero() || d.ExpiresAt.After(b.ExpiresAt) {
			return DirectoryBundle{}, ErrDirectory
		}
		ids[d.ID] = true
		for _, a := range d.Actions {
			if a != protocol.CapabilityDeploy && a != protocol.CapabilityRollback {
				return DirectoryBundle{}, ErrDirectory
			}
		}
	}
	if ctx.Err() != nil || !b.ExpiresAt.After(f.now()) || f.Ledger.AcceptIdentityDirectory(ctx, b.TenantID, b.Issuer, b.Revision, hash) != nil || !b.ExpiresAt.After(f.now()) || ctx.Err() != nil {
		return DirectoryBundle{}, ErrDirectory
	}
	return b, nil
}
func (f *FileDirectory) Member(ctx context.Context, issuer, subject string) (DirectoryMember, error) {
	b, err := f.Read(ctx)
	if err != nil {
		return DirectoryMember{}, ErrDirectory
	}
	for _, m := range b.Members {
		if m.Issuer == issuer && m.Subject == subject && m.Enabled {
			m.Roles = slices.Clone(m.Roles)
			return m, nil
		}
	}
	return DirectoryMember{}, ErrDirectory
}
func (f *FileDirectory) ResolveDelegation(ctx context.Context, reference string) (Delegation, error) {
	b, err := f.Read(ctx)
	if err != nil {
		return Delegation{}, ErrDirectory
	}
	for _, d := range b.Delegations {
		if d.ID == reference && d.ExpiresAt.After(f.now()) {
			for _, m := range b.Members {
				if m.Enabled && m.Issuer == d.Issuer && m.Subject == d.UserSubject {
					return copyDelegation(d), nil
				}
			}
		}
	}
	return Delegation{}, ErrDirectory
}
func (f *FileDirectory) now() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}
