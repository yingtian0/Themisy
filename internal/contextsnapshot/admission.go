package contextsnapshot

import (
	"context"
	"crypto/ed25519"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"themisy/internal/contract"
	"themisy/internal/grant"
	"themisy/internal/profile"
	"themisy/pkg/adapter"
	"themisy/pkg/protocol"
	"time"
)

const AdmissionVersion = "themisy.source-admission/v1"

type MetadataPayload struct {
	Record   MetadataRecord `json:"record"`
	Contract string         `json:"contract"`
	Profile  string         `json:"profile"`
}
type AdmissionTarget struct {
	Source SourceContextRecord `json:"source"`
	Build  BuildStatement      `json:"build"`
}
type ObservationRecord struct {
	Service      string            `json:"service"`
	Environment  string            `json:"environment"`
	CI           *CIRecord         `json:"ci,omitempty"`
	Dependencies *DependencyRecord `json:"dependencies,omitempty"`
	Metadata     *MetadataRecord   `json:"metadata,omitempty"`
}
type AdmissionBundle struct {
	Observations []ObservationRecord        `json:"observations,omitempty"`
	Version      string                     `json:"version"`
	Issuer       string                     `json:"issuer"`
	TenantID     string                     `json:"tenant_id"`
	Revision     int64                      `json:"revision"`
	Status       adapter.VerificationStatus `json:"status"`
	ReasonCode   string                     `json:"reason_code"`
	IssuedAt     time.Time                  `json:"issued_at"`
	ExpiresAt    time.Time                  `json:"expires_at"`
	Catalog      []MetadataPayload          `json:"catalog,omitempty"`
	Targets      []AdmissionTarget          `json:"targets,omitempty"`
	Hash         string                     `json:"hash"`
	Signature    protocol.Signature         `json:"signature"`
}

func admissionHash(b AdmissionBundle) (string, error) {
	b.Hash = ""
	b.Signature.Value = ""
	b.IssuedAt = b.IssuedAt.UTC()
	b.ExpiresAt = b.ExpiresAt.UTC()
	return digest(b)
}
func admissionMessage(b AdmissionBundle) ([]byte, error) {
	hash, err := admissionHash(b)
	if err != nil {
		return nil, err
	}
	return []byte(AdmissionVersion + "\x00" + hash), nil
}

// SignAdmission accepts opaque collector outputs, never exported positive records.
func SignAdmission(ctx context.Context, signer grant.Signer, keyID, issuer, tenant string, revision int64, maximumAge time.Duration, snapshots []SourceContextSnapshot, catalog []MetadataSnapshot, builds []BuildStatement, now time.Time) (AdmissionBundle, error) {
	if len(snapshots) == 0 || len(snapshots) != len(builds) || len(catalog) == 0 {
		return AdmissionBundle{}, ErrSourceContext
	}
	b := AdmissionBundle{Version: AdmissionVersion, Issuer: issuer, TenantID: tenant, Revision: revision, Status: adapter.VerificationPass, ReasonCode: "SOURCES_VERIFIED", IssuedAt: now.UTC(), ExpiresAt: now.UTC().Add(maximumAge)}
	for _, metadata := range catalog {
		if metadata.RequireFreshMetadata(now) != nil || metadata.record.TenantID != tenant {
			return AdmissionBundle{}, ErrSourceContext
		}
		b.Catalog = append(b.Catalog, MetadataPayload{Record: metadata.Record(), Contract: metadata.contractBytes, Profile: metadata.profileBytes})
		for _, item := range []MetadataItem{metadata.record.Contract, metadata.record.Profile} {
			if until := item.FetchedAt.Add(item.MaximumAge); until.Before(b.ExpiresAt) {
				b.ExpiresAt = until
			}
		}
	}
	for i, snapshot := range snapshots {
		if snapshot.RequireFreshSources(now) != nil || snapshot.record.TenantID != tenant {
			return AdmissionBundle{}, ErrSourceContext
		}
		build := builds[i]
		record := snapshot.Record()
		if build.Service != record.Target.Service || build.TenantID != tenant || build.Repository != record.CI.Evidence.Repository || build.CommitSHA != record.CommitSHA || build.Workflow != record.CI.Evidence.Workflow || build.RunID != record.CI.Evidence.RunID || build.RunAttempt != record.CI.Evidence.RunAttempt {
			return AdmissionBundle{}, ErrSourceContext
		}
		b.Targets = append(b.Targets, AdmissionTarget{Source: record, Build: build})
		for _, until := range []time.Time{record.ExpiresAt, build.ExpiresAt} {
			if until.Before(b.ExpiresAt) {
				b.ExpiresAt = until
			}
		}
	}
	slices.SortFunc(b.Catalog, func(a, c MetadataPayload) int {
		return compareScope(a.Record.Service, a.Record.Environment, c.Record.Service, c.Record.Environment)
	})
	slices.SortFunc(b.Targets, func(a, c AdmissionTarget) int {
		return compareScope(a.Source.Target.Service, a.Source.Target.Environment, c.Source.Target.Service, c.Source.Target.Environment)
	})
	return signAdmission(ctx, signer, keyID, b)
}
func compareScope(a, b, c, d string) int {
	if a < c {
		return -1
	}
	if a > c {
		return 1
	}
	if b < d {
		return -1
	}
	if b > d {
		return 1
	}
	return 0
}

// A failed collection publishes an explicit short-lived negative revision. It
// cannot sign a caller-asserted PASS or reuse an earlier healthy source record.
func SignUnavailableAdmission(ctx context.Context, signer grant.Signer, keyID, issuer, tenant string, revision int64, status adapter.VerificationStatus, now time.Time) (AdmissionBundle, error) {
	if !status.Valid() || status == adapter.VerificationPass {
		return AdmissionBundle{}, ErrSourceContext
	}
	return signAdmission(ctx, signer, keyID, AdmissionBundle{Version: AdmissionVersion, Issuer: issuer, TenantID: tenant, Revision: revision, Status: status, ReasonCode: "SOURCE_COLLECTION_UNAVAILABLE", IssuedAt: now.UTC(), ExpiresAt: now.UTC().Add(time.Minute)})
}
func signAdmission(ctx context.Context, signer grant.Signer, keyID string, b AdmissionBundle) (AdmissionBundle, error) {
	if signer == nil || keyID == "" || b.Issuer == "" || b.TenantID == "" || b.Revision < 1 || b.IssuedAt.IsZero() || !b.ExpiresAt.After(b.IssuedAt) || b.ExpiresAt.Sub(b.IssuedAt) > 15*time.Minute || ctx.Err() != nil {
		return AdmissionBundle{}, ErrSourceContext
	}
	b.Signature = protocol.Signature{Algorithm: "Ed25519", KeyID: keyID}
	var err error
	b.Hash, err = admissionHash(b)
	if err != nil {
		return AdmissionBundle{}, err
	}
	message, err := admissionMessage(b)
	if err != nil {
		return AdmissionBundle{}, err
	}
	b.Signature, err = signer.Sign(ctx, message)
	if err != nil || b.Signature.Algorithm != "Ed25519" || b.Signature.KeyID != keyID || ctx.Err() != nil {
		return AdmissionBundle{}, ErrSourceContext
	}
	return b, nil
}
func verifyAdmissionSignature(ctx context.Context, b AdmissionBundle, issuer, tenant string, keys grant.KeyResolver, now time.Time) error {
	if keys == nil || ctx.Err() != nil || b.Version != AdmissionVersion || b.Issuer != issuer || b.TenantID != tenant || b.Revision < 1 || !b.Status.Valid() || b.Signature.Algorithm != "Ed25519" || b.IssuedAt.IsZero() || b.IssuedAt.After(now) || !b.ExpiresAt.After(b.IssuedAt) || b.ExpiresAt.Sub(b.IssuedAt) > 15*time.Minute {
		return ErrSourceContext
	}
	hash, err := admissionHash(b)
	if err != nil || hash != b.Hash {
		return ErrSourceContext
	}
	key, err := keys.ResolveGrantKey(ctx, issuer, b.Signature.KeyID)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return ErrSourceContext
	}
	message, err := admissionMessage(b)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(b.Signature.Value)
	if err != nil || !ed25519.Verify(key, message, sig) || ctx.Err() != nil {
		return ErrSourceContext
	}
	return nil
}
func restoreMetadata(p MetadataPayload, target MetadataTarget, now time.Time) (MetadataSnapshot, error) {
	r := p.Record
	if r.Contract.Repository != target.Owner+"/"+target.Repository || r.Profile.Repository != r.Contract.Repository || r.Contract.Revision != target.Revision || r.Profile.Revision != target.Revision || r.Contract.Path != target.ContractPath || r.Profile.Path != target.ProfilePath || r.Contract.MaximumAge > target.MaximumAge || r.Profile.MaximumAge > target.MaximumAge {
		return MetadataSnapshot{}, ErrSourceContext
	}
	for _, item := range []struct {
		body   string
		record MetadataItem
	}{{p.Contract, r.Contract}, {p.Profile, r.Profile}} {
		blob := sha1.New()
		fmt.Fprintf(blob, "blob %d\x00", len(item.body))
		blob.Write([]byte(item.body))
		if metadataDigest([]byte(item.body)) != item.record.RawHash || hex.EncodeToString(blob.Sum(nil)) != item.record.BlobSHA {
			return MetadataSnapshot{}, ErrSourceContext
		}
	}
	snapshot := MetadataSnapshot{record: r, contractBytes: p.Contract, profileBytes: p.Profile}
	if snapshot.RequireFreshMetadata(now) != nil {
		return MetadataSnapshot{}, ErrSourceContext
	}
	return snapshot, nil
}

type VerifiedAdmission struct {
	bundle    AdmissionBundle
	contracts []contract.ServiceContract
	profiles  []profile.ReleaseProfile
}

func (v VerifiedAdmission) Hash() string            { return v.bundle.Hash }
func (v VerifiedAdmission) Bundle() AdmissionBundle { return cloneAdmission(v.bundle) }
func (v VerifiedAdmission) Contracts() []contract.ServiceContract {
	result := make([]contract.ServiceContract, len(v.contracts))
	for i, c := range v.contracts {
		raw, _ := json.Marshal(c)
		json.Unmarshal(raw, &result[i])
		result[i].ContentHash = c.ContentHash
		result[i].Source = c.Source
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Metadata.Name < result[j].Metadata.Name })
	return result
}
func (v VerifiedAdmission) Profiles() []profile.ReleaseProfile {
	result := make([]profile.ReleaseProfile, len(v.profiles))
	for i, p := range v.profiles {
		raw, _ := json.Marshal(p)
		json.Unmarshal(raw, &result[i])
		result[i].ContentHash = p.ContentHash
		result[i].Source = p.Source
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Metadata.Name < result[j].Metadata.Name })
	return result
}
