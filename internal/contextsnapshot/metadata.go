package contextsnapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha1" // Git blob identity only; content integrity records use SHA-256.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"themisy/adapters/githubactions"
	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/profile"
	"themisy/pkg/credentials"
)

const MetadataSchema = "themisy.metadata-context/v1alpha1"
const metadataLimit = 1 << 20

var ErrMetadataContext = errors.New("METADATA_CONTEXT_UNAVAILABLE")
var metadataRepoPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var metadataPath = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// MetadataTarget is trusted operator configuration. Revision is an immutable
// commit SHA, never a branch or tag; no request can choose a source selector.
type MetadataTarget struct {
	Owner, Repository, Revision, ContractPath, ProfilePath string
	MaximumAge                                             time.Duration
}
type MetadataConfig struct {
	Targets map[DependencyKey]MetadataTarget
}
type MetadataItem struct {
	Kind          string        `json:"kind"`
	SchemaVersion string        `json:"schema_version"`
	Name          string        `json:"name"`
	Repository    string        `json:"repository"`
	Revision      string        `json:"revision"`
	Path          string        `json:"path"`
	BlobSHA       string        `json:"blob_sha"`
	RawHash       string        `json:"raw_hash"`
	ContentHash   string        `json:"content_hash"`
	FetchedAt     time.Time     `json:"fetched_at"`
	MaximumAge    time.Duration `json:"maximum_age"`
}
type MetadataRecord struct {
	SchemaVersion string       `json:"schema_version"`
	TenantID      string       `json:"tenant_id"`
	Service       string       `json:"service"`
	Environment   string       `json:"environment"`
	CapturedAt    time.Time    `json:"captured_at"`
	Contract      MetadataItem `json:"contract"`
	Profile       MetadataItem `json:"profile"`
	Hash          string       `json:"hash"`
}

// MetadataSnapshot owns validated bytes privately. Audit/exported records are
// provenance for inspection, not a means of restoring trusted metadata.
type MetadataSnapshot struct {
	record                      MetadataRecord
	contractBytes, profileBytes string
}

func (s MetadataSnapshot) Record() MetadataRecord { return s.record }
func (s MetadataSnapshot) Hash() string           { return s.record.Hash }
func (s *MetadataSnapshot) UnmarshalJSON([]byte) error {
	if s != nil {
		*s = MetadataSnapshot{}
	}
	return ErrMetadataContext
}
func (s MetadataSnapshot) Contract() (contract.ServiceContract, error) {
	c, e := contract.Decode([]byte(s.contractBytes))
	if e != nil || c.ContentHash != s.record.Contract.ContentHash {
		return contract.ServiceContract{}, ErrMetadataContext
	}
	c.Source = metadataSource(s.record.Contract)
	return c, nil
}
func (s MetadataSnapshot) Profile() (profile.ReleaseProfile, error) {
	p, e := profile.Decode([]byte(s.profileBytes))
	if e != nil || p.ContentHash != s.record.Profile.ContentHash {
		return profile.ReleaseProfile{}, ErrMetadataContext
	}
	p.Source = metadataSource(s.record.Profile)
	return p, nil
}
func metadataSource(i MetadataItem) string {
	return "github:" + i.Repository + "@" + i.Revision + ":" + i.Path
}
func metadataHash(r MetadataRecord) (string, error) {
	r.Hash = ""
	b, e := json.Marshal(r)
	if e != nil {
		return "", e
	}
	return metadataDigest(b), nil
}
func metadataDigest(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
func (s MetadataSnapshot) RequireFreshMetadata(now time.Time) error {
	r := s.record
	h, e := metadataHash(r)
	if e != nil || now.IsZero() || r.SchemaVersion != MetadataSchema || r.Hash == "" || h != r.Hash || r.CapturedAt.IsZero() || r.CapturedAt.After(now) {
		return ErrMetadataContext
	}
	for _, i := range []MetadataItem{r.Contract, r.Profile} {
		if i.FetchedAt.IsZero() || i.FetchedAt.After(r.CapturedAt) || i.MaximumAge <= 0 || !i.FetchedAt.Add(i.MaximumAge).After(now) {
			return ErrMetadataContext
		}
	}
	if _, e := s.Contract(); e != nil {
		return ErrMetadataContext
	}
	if _, e := s.Profile(); e != nil {
		return ErrMetadataContext
	}
	return nil
}

type MetadataCollector struct {
	targets map[DependencyKey]MetadataTarget
	client  githubactions.HTTPDoer
	broker  *credentials.GitHubBroker
	audit   CIAuditSink
	now     func() time.Time
}

func NewMetadataCollector(config MetadataConfig, source credentials.GitHubTokenSource, client githubactions.HTTPDoer, sink CIAuditSink) (*MetadataCollector, error) {
	if len(config.Targets) == 0 || source == nil || sink == nil {
		return nil, ErrMetadataContext
	}
	allowed := make(map[credentials.GitHubTarget]struct{}, len(config.Targets))
	for k, t := range config.Targets {
		if strings.TrimSpace(k.TenantID) == "" || strings.TrimSpace(k.Service) == "" || strings.TrimSpace(k.Environment) == "" || !metadataRepoPart.MatchString(t.Owner) || !metadataRepoPart.MatchString(t.Repository) || !commitSHA.MatchString(t.Revision) || !validMetadataPath(t.ContractPath) || !validMetadataPath(t.ProfilePath) || t.ContractPath == t.ProfilePath || t.MaximumAge < time.Minute || t.MaximumAge > 24*time.Hour {
			return nil, ErrMetadataContext
		}
		allowed[credentials.GitHubTarget{TenantID: k.TenantID, Service: k.Service, Environment: k.Environment}] = struct{}{}
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	c := &MetadataCollector{targets: maps.Clone(config.Targets), client: client, audit: sink, now: time.Now}
	c.broker = &credentials.GitHubBroker{Source: source, Allowed: allowed, Now: func() time.Time { return c.now().UTC() }}
	return c, nil
}
func validMetadataPath(p string) bool {
	if len(p) == 0 || len(p) > 512 || !metadataPath.MatchString(p) || path.IsAbs(p) || path.Clean(p) != p || p == "." || strings.HasPrefix(p, "../") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return true
}
func (c *MetadataCollector) Collect(ctx context.Context, key DependencyKey) (MetadataSnapshot, error) {
	if c == nil || c.client == nil || c.broker == nil || c.audit == nil || ctx.Err() != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	target, ok := c.targets[key]
	if !ok {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	started := c.now().UTC()
	if started.IsZero() {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	cred, e := c.broker.Acquire(ctx, credentials.Request{Provider: credentials.GitHubProvider, TenantID: key.TenantID, Service: key.Service, Environment: key.Environment, Purpose: credentials.PurposeVerify})
	if e != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	contractRaw, contractItem, e := c.read(ctx, target, target.ContractPath, cred)
	if e != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	serviceContract, e := contract.Decode(contractRaw)
	if e != nil || serviceContract.Metadata.Name != key.Service {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	environment, ok := serviceContract.Environments[key.Environment]
	if !ok {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	profileRaw, profileItem, e := c.read(ctx, target, target.ProfilePath, cred)
	if e != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	releaseProfile, e := profile.Decode(profileRaw)
	if e != nil || releaseProfile.Metadata.Name != environment.ReleaseProfile {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	capabilities := make(map[domain.Capability]bool)
	for _, capability := range environment.Capabilities {
		capabilities[capability] = true
	}
	for _, required := range releaseProfile.Spec.RequiredCapabilities {
		if !capabilities[required] {
			return MetadataSnapshot{}, ErrMetadataContext
		}
	}
	contractItem.Kind = serviceContract.Kind
	contractItem.SchemaVersion = serviceContract.APIVersion
	contractItem.Name = serviceContract.Metadata.Name
	contractItem.ContentHash = serviceContract.ContentHash
	profileItem.Kind = releaseProfile.Kind
	profileItem.SchemaVersion = releaseProfile.APIVersion
	profileItem.Name = releaseProfile.Metadata.Name
	profileItem.ContentHash = releaseProfile.ContentHash
	r := MetadataRecord{SchemaVersion: MetadataSchema, TenantID: key.TenantID, Service: key.Service, Environment: key.Environment, CapturedAt: c.now().UTC(), Contract: contractItem, Profile: profileItem}
	if r.CapturedAt.Before(started) || !cred.ValidAt(r.CapturedAt) || ctx.Err() != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	r.Hash, e = metadataHash(r)
	if e != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	s := MetadataSnapshot{record: r, contractBytes: string(contractRaw), profileBytes: string(profileRaw)}
	if s.RequireFreshMetadata(r.CapturedAt) != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	id := "metadata_observation_" + rand.Text()
	event := domain.AuditEvent{ID: id, CorrelationID: "context/" + key.TenantID + "/" + id, ActorType: "system", ActorID: "metadata-collector", Action: "context.metadata.observe", ResourceType: "metadata_snapshot", ResourceID: r.Hash, Result: "PASS", Timestamp: r.CapturedAt, Details: map[string]any{"record": r}}
	appendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if c.audit.AppendAuditContext(appendCtx, event) != nil || appendCtx.Err() != nil || ctx.Err() != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	// Unlike raw observations, this unit returns decoded usable metadata. Recheck
	// freshness after append so slow durable storage cannot return expired input.
	if s.RequireFreshMetadata(c.now().UTC()) != nil {
		return MetadataSnapshot{}, ErrMetadataContext
	}
	return s, nil
}
func (c *MetadataCollector) read(ctx context.Context, t MetadataTarget, p string, cred credentials.Credential) ([]byte, MetadataItem, error) {
	started := c.now().UTC()
	if started.IsZero() || !cred.ValidAt(started) {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	endpoint := "https://api.github.com/repos/" + t.Owner + "/" + t.Repository + "/contents/" + p + "?ref=" + url.QueryEscape(t.Revision)
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if e != nil {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+string(cred.Value("token")))
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, e := c.client.Do(req)
	if e != nil || response == nil || response.Body == nil {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	body, e := io.ReadAll(io.LimitReader(response.Body, 2*metadataLimit+1))
	if e != nil || len(body) > 2*metadataLimit {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	var file struct {
		Type, Path, SHA, Encoding, Content string
		Size                               int64
		Target                             string
		SubmoduleGitURL                    string `json:"submodule_git_url"`
	}
	if json.Unmarshal(body, &file) != nil || file.Type != "file" || file.Path != p || file.Encoding != "base64" || !commitSHA.MatchString(file.SHA) || file.Target != "" || file.SubmoduleGitURL != "" || file.Size <= 0 || file.Size > metadataLimit {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	raw, e := base64.StdEncoding.Strict().DecodeString(file.Content)
	if e != nil || int64(len(raw)) != file.Size {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	blob := sha1.New()
	fmt.Fprintf(blob, "blob %d\x00", len(raw))
	_, _ = blob.Write(raw)
	if hex.EncodeToString(blob.Sum(nil)) != file.SHA {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	at := c.now().UTC()
	if ctx.Err() != nil || at.Before(started) || !cred.ValidAt(at) {
		return nil, MetadataItem{}, ErrMetadataContext
	}
	return raw, MetadataItem{Repository: t.Owner + "/" + t.Repository, Revision: t.Revision, Path: p, BlobSHA: file.SHA, RawHash: metadataDigest(raw), FetchedAt: at, MaximumAge: t.MaximumAge}, nil
}
