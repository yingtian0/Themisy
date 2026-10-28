package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"themisy/internal/policy"
)

func (s *RunnerServer) policyCurrent(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.Policies == nil {
		http.Error(w, "POLICY_DISTRIBUTION_UNAVAILABLE", 503)
		return
	}
	delivery, err := s.Policies.Current(r.Context(), actor.TenantID, actor.RunnerGroup)
	if err != nil {
		http.Error(w, "POLICY_DISTRIBUTION_UNAVAILABLE", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(delivery)
}
func (s *RunnerServer) policyAck(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.Policies == nil {
		http.Error(w, "POLICY_DISTRIBUTION_UNAVAILABLE", 503)
		return
	}
	var input struct {
		Revision   int64  `json:"revision"`
		BundleHash string `json:"bundle_hash"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "POLICY_REQUEST_INVALID", 400)
		return
	}
	if err := s.Policies.Acknowledge(r.Context(), actor.TenantID, actor.RunnerGroup, actor.RunnerID, input.Revision, input.BundleHash); err != nil {
		http.Error(w, "POLICY_ACK_REJECTED", 409)
		return
	}
	w.WriteHeader(204)
}
func (c *RunnerClient) FetchPolicy(ctx context.Context) (policy.Delivery, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.BaseURL, "/")+"/v1/runner/policy", nil)
	if err != nil {
		return policy.Delivery{}, policy.ErrDistribution
	}
	c.authorize(req)
	response, err := c.http().Do(req)
	if err != nil {
		return policy.Delivery{}, policy.ErrDistribution
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return policy.Delivery{}, policy.ErrDistribution
	}
	var delivery policy.Delivery
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&delivery) != nil || decoder.Decode(new(any)) != io.EOF || delivery.Assignment.TenantID != c.TenantID || delivery.Assignment.RunnerGroup != c.RunnerGroup {
		return policy.Delivery{}, policy.ErrDistribution
	}
	return delivery, nil
}
func (c *RunnerClient) AcknowledgePolicy(ctx context.Context, a policy.Assignment) error {
	return c.post(ctx, "/v1/runner/policy/ack", struct {
		Revision   int64  `json:"revision"`
		BundleHash string `json:"bundle_hash"`
	}{a.Revision, a.BundleHash})
}
