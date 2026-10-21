package transport

import (
	"context"
	"net/http"
	"net/url"
	"themisy/internal/domain"
	"themisy/internal/resultreport"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

type recoveryReport struct {
	ProtocolVersion string                `json:"protocol_version"`
	RequestHash     string                `json:"request_hash"`
	Result          protocol.Result       `json:"result"`
	Report          protocol.ResultReport `json:"report"`
}

// Recovery is authenticated by the registered Runner group, not a delivery
// lease. It only reports journal/provider observations and cannot authorize a
// new write. This path also works after a Grant or delivery lease has expired.
func (s *RunnerServer) recovery(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var body recoveryReport
	if !decodeRunnerJSON(w, r, &body) {
		return
	}
	if body.ProtocolVersion != protocol.RunnerTransportVersionV1Alpha1 || body.Result.ProtocolVersion != protocol.VersionV1Alpha1 || body.Result.CompletedAt.IsZero() || !validResultStatus(body.Result.Status) {
		writeRunnerError(w, 422, "invalid recovery report")
		return
	}
	body.Result.CompletedAt = body.Result.CompletedAt.UTC()
	body.Report.Result.CompletedAt = body.Report.Result.CompletedAt.UTC()
	record, err := s.Store.GetGrantDispatch(r.Context(), r.PathValue("id"))
	if err != nil {
		writeGrantStoreError(w, err)
		return
	}
	if record.Grant.TenantID != identity.TenantID || record.Grant.RunnerGroup != identity.RunnerGroup {
		s.rejectReport(w, r, identity, record.Grant, "grant.reconcile")
		return
	}
	if !protocol.ResultEqual(body.Result, body.Report.Result) || body.RequestHash != body.Report.RequestHash || s.ResultVerifier.Verify(r.Context(), resultreport.Identity{RunnerID: identity.RunnerID, TenantID: identity.TenantID, RunnerGroup: identity.RunnerGroup}, record.Grant, body.Report) != nil {
		s.rejectReport(w, r, identity, record.Grant, "grant.reconcile")
		return
	}
	hash, hashErr := protocol.GrantHash(record.Grant)
	now := s.now()
	audit := domain.AuditEvent{ID: auditID(record.Grant.GrantID, now), CorrelationID: record.Grant.RunID, ActorType: "runner", ActorID: identity.RunnerID, Action: "grant.reconcile", ResourceType: "action_grant", ResourceID: record.Grant.GrantID, Result: "RECONCILED", Timestamp: now, Details: map[string]any{
		"signed_report": body.Report, "previous_status": record.Status, "observed_status": body.Result.Status,
		"stored_result_status": record.Result.Status, "stored_external_execution_id": record.Result.ExternalExecutionID,
		"reported_result_status": body.Result.Status, "reported_external_execution_id": body.Result.ExternalExecutionID,
	}}
	if hashErr != nil || hash != body.RequestHash || record.Grant.GrantID != body.Result.GrantID {
		audit.Result = "CONFLICT"
		if err := s.appendAudit(audit); err != nil {
			writeRunnerError(w, http.StatusServiceUnavailable, "audit store unavailable")
			return
		}
		writeRunnerError(w, 409, "journal/dispatch mismatch")
		return
	}
	if err = s.Store.ReconcileGrantDispatch(r.Context(), record.Grant.GrantID, record.StateVersion, body.Result, audit); err != nil {
		audit.Result = "CONFLICT"
		if err := s.appendAudit(audit); err != nil {
			writeRunnerError(w, http.StatusServiceUnavailable, "audit store unavailable")
			return
		}
		writeGrantStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (c *RunnerClient) ReportRecovery(ctx context.Context, r store.RunnerActionRecord) error {
	report, err := c.ResultSigner.Sign(ctx, resultreport.Identity{RunnerID: c.RunnerID, TenantID: c.TenantID, RunnerGroup: c.RunnerGroup}, r.RequestHash, r.Result)
	if err != nil {
		return err
	}
	return c.post(ctx, "/v1/runner/grants/"+url.PathEscape(r.GrantID)+"/reconcile", recoveryReport{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, RequestHash: r.RequestHash, Result: report.Result, Report: report})
}
