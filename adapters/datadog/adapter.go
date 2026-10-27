package datadog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"time"

	"themisy/pkg/adapter"
	"themisy/pkg/credentials"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Adapter struct {
	config Config
	client HTTPDoer
	now    func() time.Time
}

func New(config Config, client HTTPDoer) (*Adapter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Adapter{config: config, client: client, now: time.Now}, nil
}

func (a *Adapter) Name() string    { return AdapterName }
func (a *Adapter) Version() string { return AdapterVersion }

func (a *Adapter) Verify(ctx context.Context, request adapter.VerificationRequest, credential credentials.Credential) (adapter.Evidence, error) {
	query, ok := a.config.Queries[QueryKey{Service: request.Target.Service, Environment: request.Target.Environment}]
	if !ok {
		return a.evidence(request, adapter.VerificationMissing, "QUERY_NOT_CONFIGURED", Query{}, 0, a.now()), nil
	}
	if request.Window.From.IsZero() || request.Window.To.IsZero() || !request.Window.To.After(request.Window.From) || request.Deployment.ExternalExecutionID == "" {
		return adapter.Evidence{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: "verify.validate", Err: errors.New("valid window and deployment are required")}
	}
	if !credential.ValidAt(a.now()) || len(credential.Value("api_key")) == 0 || len(credential.Value("app_key")) == 0 {
		return adapter.Evidence{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: "verify.credential", Err: errors.New("valid Datadog API and application keys are required")}
	}
	baseURL, _ := a.config.Site.apiURL()
	parameters := url.Values{}
	parameters.Set("from", fmt.Sprintf("%d", request.Window.From.UTC().Unix()))
	parameters.Set("to", fmt.Sprintf("%d", request.Window.To.UTC().Unix()))
	parameters.Set("query", query.Expression)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/query?"+parameters.Encode(), nil)
	if err != nil {
		return adapter.Evidence{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: "verify.request", Err: err}
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("DD-API-KEY", string(credential.Value("api_key")))
	httpRequest.Header.Set("DD-APPLICATION-KEY", string(credential.Value("app_key")))
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return a.evidence(request, adapter.VerificationInconclusive, "SOURCE_UNAVAILABLE", query, 0, a.now()), nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			return a.evidence(request, adapter.VerificationInconclusive, "SOURCE_UNAVAILABLE", query, 0, a.now()), nil
		}
		return adapter.Evidence{}, &adapter.Error{Class: adapter.ErrorTerminal, Operation: "verify.query", Err: fmt.Errorf("Datadog returned HTTP %d", response.StatusCode)}
	}
	var payload struct {
		Status string `json:"status"`
		Series []struct {
			Points [][]*float64 `json:"pointlist"`
		} `json:"series"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload); err != nil || payload.Status != "ok" {
		return a.evidence(request, adapter.VerificationInconclusive, "MALFORMED_RESPONSE", query, 0, a.now()), nil
	}
	points := flattenPoints(payload.Series)
	if len(points) == 0 {
		return a.evidence(request, adapter.VerificationMissing, "NO_SERIES", query, 0, a.now()), nil
	}
	if len(points) < query.MinimumPoints {
		return a.evidence(request, adapter.VerificationInconclusive, "INSUFFICIENT_POINTS", query, aggregate(points, query.Aggregation), a.now()), nil
	}
	latest := points[len(points)-1].at
	if request.Window.To.Sub(latest) > query.MaximumAge || latest.Before(request.Window.From) || latest.After(request.Window.To) || latest.After(a.now()) {
		return a.sampleEvidence(request, adapter.VerificationInconclusive, "STALE_SERIES", query, aggregate(points, query.Aggregation), latest), nil
	}
	value := aggregate(points, query.Aggregation)
	status := compare(value, query)
	reason := "THRESHOLD_MET"
	if status == adapter.VerificationFail {
		reason = "THRESHOLD_BREACHED"
	}
	return a.sampleEvidence(request, status, reason, query, value, latest), nil
}

type point struct {
	at    time.Time
	value float64
}

func flattenPoints(series []struct {
	Points [][]*float64 `json:"pointlist"`
}) []point {
	result := make([]point, 0)
	for _, item := range series {
		for _, raw := range item.Points {
			if len(raw) != 2 || raw[0] == nil || raw[1] == nil || math.IsNaN(*raw[0]) || math.IsInf(*raw[0], 0) || math.IsNaN(*raw[1]) || math.IsInf(*raw[1], 0) {
				continue
			}
			result = append(result, point{at: time.UnixMilli(int64(*raw[0])).UTC(), value: *raw[1]})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].at.Before(result[j].at) })
	return result
}

func aggregate(points []point, mode string) float64 {
	if len(points) == 0 {
		return 0
	}
	if mode == "last" {
		return points[len(points)-1].value
	}
	value := points[0].value
	if mode == "avg" {
		value = 0
	}
	for _, item := range points {
		if mode == "avg" {
			value += item.value
		} else if mode == "max" && item.value > value {
			value = item.value
		}
	}
	if mode == "avg" {
		value /= float64(len(points))
	}
	return value
}

func (a *Adapter) evidence(request adapter.VerificationRequest, status adapter.VerificationStatus, reason string, query Query, value float64, observedAt time.Time) adapter.Evidence {
	queryDigest := sha256.Sum256([]byte(query.Expression))
	evidence := adapter.Evidence{Query: query.Expression, Status: status, ReasonCode: reason, Source: AdapterName, QueryHash: "sha256:" + hex.EncodeToString(queryDigest[:]), Window: request.Window, ObservedAt: observedAt.UTC(), ObservedValue: value, Threshold: query.Threshold, AdapterVersion: AdapterVersion}
	evidence.EvidenceHash, _ = adapter.EvidenceHash(evidence)
	return evidence
}

// Collection time and metric sample time are distinct. A recent historical
// datapoint is not evidence that collection occurred before the window ended.
func (a *Adapter) sampleEvidence(request adapter.VerificationRequest, status adapter.VerificationStatus, reason string, query Query, value float64, sampled time.Time) adapter.Evidence {
	e := a.evidence(request, status, reason, query, value, a.now())
	e.SampledAt = sampled.UTC().Format(time.RFC3339Nano)
	e.EvidenceHash, _ = adapter.EvidenceHash(e)
	return e
}
