package api

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"themisy/internal/identity"
	"themisy/internal/policy"
)

func PolicyHandler(distributor *policy.Distributor) http.Handler {
	if distributor == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 503, errorResponse{Error: "POLICY_DISTRIBUTION_UNAVAILABLE"})
		})
	}
	routes := http.NewServeMux()
	decode := func(w http.ResponseWriter, r *http.Request, value any) bool {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		decoder.DisallowUnknownFields()
		if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
			writeJSON(w, 400, errorResponse{Error: "POLICY_REQUEST_INVALID"})
			return false
		}
		return true
	}
	actor := func(w http.ResponseWriter, r *http.Request) (identity.RequestActor, bool) {
		a, ok := identity.RequestActorFromContext(r.Context())
		if !ok || !slices.Contains(a.Roles, "policy-admin") {
			writeJSON(w, 403, errorResponse{Error: "POLICY_ADMIN_REQUIRED"})
			return identity.RequestActor{}, false
		}
		return a, true
	}
	fail := func(w http.ResponseWriter, err error) {
		if err == policy.ErrAssignmentConflict {
			writeJSON(w, 409, errorResponse{Error: "POLICY_ASSIGNMENT_CONFLICT"})
		} else {
			writeJSON(w, 503, errorResponse{Error: "POLICY_DISTRIBUTION_UNAVAILABLE"})
		}
	}
	routes.HandleFunc("POST /v1/policy/bundles", func(w http.ResponseWriter, r *http.Request) {
		a, ok := actor(w, r)
		if !ok {
			return
		}
		var b policy.Bundle
		if !decode(w, r, &b) {
			return
		}
		hash, err := distributor.Register(r.Context(), a.TenantID, a.Subject, a.Issuer, b)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"bundle_hash": hash})
	})
	routes.HandleFunc("GET /v1/policy/bundles/{hash}", func(w http.ResponseWriter, r *http.Request) {
		a, ok := actor(w, r)
		if !ok {
			return
		}
		b, err := distributor.Bundle(r.Context(), a.TenantID, r.PathValue("hash"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, b)
	})
	routes.HandleFunc("GET /v1/policy/assignments/{group}", func(w http.ResponseWriter, r *http.Request) {
		a, ok := actor(w, r)
		if !ok {
			return
		}
		if a.TenantID != distributor.TenantID || !distributor.AllowedGroups[r.PathValue("group")] {
			fail(w, policy.ErrDistribution)
			return
		}
		assignment, err := distributor.Store.GetPolicyAssignment(r.Context(), a.TenantID, r.PathValue("group"))
		if err != nil {
			fail(w, err)
			return
		}
		state, reason := "AVAILABLE", "POLICY_ASSIGNMENT_VERIFIED"
		if _, err := distributor.Current(r.Context(), a.TenantID, r.PathValue("group")); err != nil {
			state, reason = "UNAVAILABLE", "POLICY_BUNDLE_UNAVAILABLE"
		}
		acks, err := distributor.Store.PolicyAcknowledgements(r.Context(), a.TenantID, r.PathValue("group"), assignment.Revision)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"assignment": assignment, "status": state, "reason_code": reason, "acknowledgements": acks, "acknowledgement_meaning": "DELIVERY_VERIFIED_ONLY"})
	})
	routes.HandleFunc("PUT /v1/policy/assignments/{group}", func(w http.ResponseWriter, r *http.Request) {
		a, ok := actor(w, r)
		if !ok {
			return
		}
		var input struct {
			BundleHash       string `json:"bundle_hash"`
			ExpectedRevision int64  `json:"expected_revision"`
		}
		if !decode(w, r, &input) {
			return
		}
		assignment, err := distributor.Assign(r.Context(), a.TenantID, r.PathValue("group"), input.BundleHash, a.Subject, a.Issuer, input.ExpectedRevision)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, assignment)
	})
	routes.HandleFunc("POST /v1/policy/assignments/{group}/rollback", func(w http.ResponseWriter, r *http.Request) {
		a, ok := actor(w, r)
		if !ok {
			return
		}
		var input struct {
			ExpectedRevision int64 `json:"expected_revision"`
		}
		if !decode(w, r, &input) {
			return
		}
		assignment, err := distributor.Rollback(r.Context(), a.TenantID, r.PathValue("group"), a.Subject, a.Issuer, input.ExpectedRevision)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, assignment)
	})
	routes.HandleFunc("POST /v1/policy/assignments/{group}/shadow", func(w http.ResponseWriter, r *http.Request) {
		a, ok := actor(w, r)
		if !ok {
			return
		}
		var input struct {
			CandidateHash string              `json:"candidate_hash"`
			Cases         []policy.ShadowCase `json:"cases"`
		}
		if !decode(w, r, &input) {
			return
		}
		current, err := distributor.Current(r.Context(), a.TenantID, r.PathValue("group"))
		if err != nil {
			fail(w, err)
			return
		}
		candidate, err := distributor.Bundle(r.Context(), a.TenantID, input.CandidateHash)
		if err != nil {
			fail(w, err)
			return
		}
		sink, ok := distributor.Store.(policy.ShadowAuditSink)
		if !ok {
			fail(w, policy.ErrShadowAudit)
			return
		}
		comparator, err := policy.NewAuditedShadowComparator(distributor.Issuer, distributor.Keys, sink)
		if err != nil {
			fail(w, err)
			return
		}
		report, err := comparator.Compare(r.Context(), current.Bundle, candidate, input.Cases)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, report)
	})
	return routes
}
