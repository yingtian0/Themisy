package datadog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

const HealthSchema = "themisy.dependency-health/v1alpha1"
const healthResponseLimit = 2 << 20

var ErrHealthUnavailable = errors.New("DEPENDENCY_HEALTH_UNAVAILABLE")

type HealthTargetKey struct{ TenantID, Service, Environment string }

// HealthTarget is trusted operator configuration. No caller selects a site,
// expression, threshold, observation window, or freshness allowance.
type HealthTarget struct {
	Site   Site
	Query  Query
	Window time.Duration
}

type HealthConfig struct {
	Targets map[HealthTargetKey]HealthTarget
}

type HealthRequest struct {
	TenantID string
	Target   adapter.Target
}

// HealthEvidence is an inspectable observation, not trusted input, a complete
// dependency snapshot, a deployment proof, or authority to issue a Grant.
type HealthEvidence struct {
	SchemaVersion   string                     `json:"schema_version"`
	TenantID        string                     `json:"tenant_id"`
	Target          adapter.Target             `json:"target"`
	Status          adapter.VerificationStatus `json:"status"`
	ReasonCode      string                     `json:"reason_code"`
	Source          string                     `json:"source"`
	Site            Site                       `json:"site"`
	QueryHash       string                     `json:"query_hash"`
	Window          adapter.ObservationWindow  `json:"window"`
	CapturedAt      time.Time                  `json:"captured_at"`
	SourceUpdatedAt time.Time                  `json:"source_updated_at,omitempty"`
	MaximumAge      time.Duration              `json:"maximum_age"`
	ObservedValue   float64                    `json:"observed_value,omitempty"`
	SamplesHash     string                     `json:"samples_hash,omitempty"`
	SampleCount     int                        `json:"sample_count,omitempty"`
	EvidenceHash    string                     `json:"evidence_hash"`
}

type HealthReader struct {
	targets map[HealthTargetKey]HealthTarget
	client  HTTPDoer
	now     func() time.Time
}

func NewHealthReader(config HealthConfig, client HTTPDoer) (*HealthReader, error) {
	if len(config.Targets) == 0 {
		return nil, ErrHealthUnavailable
	}
	for key, target := range config.Targets {
		q := target.Query
		if strings.TrimSpace(key.TenantID) == "" || strings.TrimSpace(key.Service) == "" || strings.TrimSpace(key.Environment) == "" ||
			target.Window < time.Minute || target.Window > 24*time.Hour || target.Window%time.Second != 0 ||
			q.MaximumAge < time.Second || q.MaximumAge > target.Window || q.MinimumPoints > 10000 ||
			math.IsNaN(q.Threshold) || math.IsInf(q.Threshold, 0) {
			return nil, ErrHealthUnavailable
		}
		if err := (Config{Site: target.Site, Queries: map[QueryKey]Query{{Service: key.Service, Environment: key.Environment}: q}}).Validate(); err != nil {
			return nil, ErrHealthUnavailable
		}
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &HealthReader{targets: maps.Clone(config.Targets), client: client, now: time.Now}, nil
}

// ReadHealth performs one fixed-site GET. Provider failures remain four-valued
// evidence; invalid configuration/identity/credentials return no observation.
// Injected HTTP clients are trusted customer-side dependencies, never request input.
func (r *HealthReader) ReadHealth(ctx context.Context, request HealthRequest, credential credentials.Credential) (HealthEvidence, error) {
	if r == nil || r.client == nil || ctx.Err() != nil {
		return HealthEvidence{}, ErrHealthUnavailable
	}
	target, ok := r.targets[HealthTargetKey{request.TenantID, request.Target.Service, request.Target.Environment}]
	if !ok {
		return HealthEvidence{}, ErrHealthUnavailable
	}
	now := r.now().UTC()
	if now.IsZero() || !credential.ValidAt(now.Add(30*time.Second)) || len(credential.Value("api_key")) == 0 || len(credential.Value("app_key")) == 0 {
		return HealthEvidence{}, ErrHealthUnavailable
	}
	// Datadog query bounds use seconds; point timestamps use milliseconds.
	to := now.Truncate(time.Second)
	window := adapter.ObservationWindow{From: to.Add(-target.Window), To: to}
	queryHash, _ := healthHash(struct {
		Key    HealthTargetKey
		Config HealthTarget
	}{HealthTargetKey{request.TenantID, request.Target.Service, request.Target.Environment}, target})
	evidence := HealthEvidence{SchemaVersion: HealthSchema, TenantID: request.TenantID, Target: request.Target, Source: AdapterName, Site: target.Site,
		QueryHash: queryHash, Window: window, MaximumAge: target.Query.MaximumAge}
	finish := func(status adapter.VerificationStatus, reason string) (HealthEvidence, error) {
		if ctx.Err() != nil {
			return HealthEvidence{}, ErrHealthUnavailable
		}
		evidence.Status = status
		evidence.ReasonCode = reason
		evidence.CapturedAt = r.now().UTC()
		if evidence.CapturedAt.Before(now) || !credential.ValidAt(evidence.CapturedAt) {
			return HealthEvidence{}, ErrHealthUnavailable
		}
		if (status == adapter.VerificationPass || status == adapter.VerificationFail) && !evidence.SourceUpdatedAt.Add(evidence.MaximumAge).After(evidence.CapturedAt) {
			evidence.Status = adapter.VerificationInconclusive
			evidence.ReasonCode = "HEALTH_SOURCE_STALE"
		}
		evidence.EvidenceHash = ""
		hash, err := healthHash(evidence)
		if err != nil {
			return HealthEvidence{}, ErrHealthUnavailable
		}
		evidence.EvidenceHash = hash
		return evidence, nil
	}
	base, _ := target.Site.apiURL()
	parameters := url.Values{"from": {strconv.FormatInt(window.From.Unix(), 10)}, "to": {strconv.FormatInt(window.To.Unix(), 10)}, "query": {target.Query.Expression}}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/query?"+parameters.Encode(), nil)
	if err != nil {
		return HealthEvidence{}, ErrHealthUnavailable
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("DD-API-KEY", string(credential.Value("api_key")))
	httpRequest.Header.Set("DD-APPLICATION-KEY", string(credential.Value("app_key")))
	response, err := r.client.Do(httpRequest)
	if err != nil || response == nil || response.Body == nil {
		return finish(adapter.VerificationInconclusive, "HEALTH_SOURCE_UNAVAILABLE")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		reason := "HEALTH_SOURCE_UNAVAILABLE"
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			reason = "HEALTH_SOURCE_AUTH_REJECTED"
		}
		if response.StatusCode == http.StatusTooManyRequests {
			reason = "HEALTH_SOURCE_RATE_LIMITED"
		}
		return finish(adapter.VerificationInconclusive, reason)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, healthResponseLimit+1))
	if err != nil || len(body) > healthResponseLimit {
		return finish(adapter.VerificationInconclusive, "HEALTH_RESPONSE_INVALID")
	}
	var payload struct {
		Status string `json:"status"`
		Query  string `json:"query"`
		From   int64  `json:"from_date"`
		To     int64  `json:"to_date"`
		Error  string `json:"error"`
		Series []struct {
			Points [][]*float64 `json:"pointlist"`
		} `json:"series"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Status != "ok" || payload.Error != "" {
		return finish(adapter.VerificationInconclusive, "HEALTH_RESPONSE_INVALID")
	}
	if payload.Query != target.Query.Expression || payload.From != window.From.UnixMilli() || payload.To != window.To.UnixMilli() {
		return finish(adapter.VerificationInconclusive, "HEALTH_SOURCE_MISMATCH")
	}
	if len(payload.Series) == 0 {
		return finish(adapter.VerificationMissing, "HEALTH_SERIES_MISSING")
	}
	// Each returned series must independently meet completeness and freshness.
	// A healthy series cannot conceal a missing or stale peer. No point is dropped
	// to obtain a positive result and a duplicate cannot satisfy MinimumPoints.
	status := adapter.VerificationPass
	seriesHashes := make([]string, 0, len(payload.Series))
	sampleCount := 0
	for _, series := range payload.Series {
		if len(series.Points) < target.Query.MinimumPoints {
			return finish(adapter.VerificationInconclusive, "HEALTH_SERIES_INCOMPLETE")
		}
		points := make([]point, 0, len(series.Points))
		seen := make(map[int64]struct{}, len(series.Points))
		latest := time.Time{}
		for _, raw := range series.Points {
			if len(raw) != 2 || raw[0] == nil || raw[1] == nil || math.IsNaN(*raw[0]) || math.IsInf(*raw[0], 0) || math.IsNaN(*raw[1]) || math.IsInf(*raw[1], 0) || math.Trunc(*raw[0]) != *raw[0] || *raw[0] < float64(window.From.UnixMilli()) || *raw[0] > float64(window.To.UnixMilli()) {
				return finish(adapter.VerificationInconclusive, "HEALTH_SERIES_INCOMPLETE")
			}
			stamp := int64(*raw[0])
			if _, duplicate := seen[stamp]; duplicate {
				return finish(adapter.VerificationInconclusive, "HEALTH_SERIES_INCOMPLETE")
			}
			seen[stamp] = struct{}{}
			at := time.UnixMilli(stamp).UTC()
			if at.After(latest) {
				latest = at
			}
			points = append(points, point{at: at, value: *raw[1]})
		}
		if !latest.Add(target.Query.MaximumAge).After(now) {
			return finish(adapter.VerificationInconclusive, "HEALTH_SOURCE_STALE")
		}
		// SourceUpdatedAt is the oldest per-series latest sample, so freshness can
		// only shrink as more dependencies/series are considered.
		if evidence.SourceUpdatedAt.IsZero() || latest.Before(evidence.SourceUpdatedAt) {
			evidence.SourceUpdatedAt = latest
		}
		sort.Slice(points, func(i, j int) bool { return points[i].at.Before(points[j].at) })
		samples := make([][2]float64, 0, len(points))
		for _, p := range points {
			samples = append(samples, [2]float64{float64(p.at.UnixMilli()), p.value})
		}
		seriesHash, err := healthHash(samples)
		if err != nil {
			return HealthEvidence{}, ErrHealthUnavailable
		}
		seriesHashes = append(seriesHashes, seriesHash)
		sampleCount += len(samples)
		value := healthAggregate(points, target.Query.Aggregation)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return finish(adapter.VerificationInconclusive, "HEALTH_SERIES_INCOMPLETE")
		}
		if compare(value, target.Query) == adapter.VerificationFail {
			status = adapter.VerificationFail
		}
		if len(payload.Series) == 1 {
			evidence.ObservedValue = value
		}
	}
	sort.Strings(seriesHashes)
	evidence.SamplesHash, err = healthHash(seriesHashes)
	if err != nil {
		return HealthEvidence{}, ErrHealthUnavailable
	}
	evidence.SampleCount = sampleCount
	reason := "HEALTH_THRESHOLD_MET"
	if status == adapter.VerificationFail {
		reason = "HEALTH_THRESHOLD_BREACHED"
	}
	return finish(status, reason)
}

func healthAggregate(points []point, mode string) float64 {
	if mode == "last" {
		latest := points[0]
		for _, p := range points[1:] {
			if p.at.After(latest.at) {
				latest = p
			}
		}
		return latest.value
	}
	return aggregate(points, mode)
}

func healthHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:]), nil
}
