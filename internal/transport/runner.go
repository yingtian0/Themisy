package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"themisy/internal/domain"
	grantvalidation "themisy/internal/grant"
	"themisy/internal/runner"
	"themisy/internal/store"
	"themisy/pkg/protocol"
)

const (
	runnerIDHeader = "X-Themisy-Runner-ID"
	defaultMaxWait = 30 * time.Second
	defaultLease   = 2 * time.Minute
)

type RunnerIdentity struct {
	RunnerID    string
	TenantID    string
	RunnerGroup string
}

type RunnerAuthenticator interface {
	Authenticate(*http.Request) (RunnerIdentity, error)
}

type RunnerRegistration struct {
	TenantID    string
	RunnerGroup string
	Token       string
}

type StaticRunnerAuthenticator map[string]RunnerRegistration

func (a StaticRunnerAuthenticator) Authenticate(request *http.Request) (RunnerIdentity, error) {
	runnerID := strings.TrimSpace(request.Header.Get(runnerIDHeader))
	registration, ok := a[runnerID]
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return RunnerIdentity{}, errors.New("runner authentication failed")
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if !ok || runnerID == "" || registration.TenantID == "" || registration.RunnerGroup == "" || registration.Token == "" || token == "" {
		return RunnerIdentity{}, errors.New("runner authentication failed")
	}
	expected, actual := sha256.Sum256([]byte(registration.Token)), sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		return RunnerIdentity{}, errors.New("runner authentication failed")
	}
	return RunnerIdentity{RunnerID: runnerID, TenantID: registration.TenantID, RunnerGroup: registration.RunnerGroup}, nil
}

type RunnerServer struct {
	Store             store.GrantDispatchStore
	Fleet             store.RunnerFleetStore
	Audit             interface{ AppendAudit(domain.AuditEvent) error }
	Auth              RunnerAuthenticator
	Now               func() time.Time
	Lease             time.Duration
	MaxWait           time.Duration
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	RequireHeartbeat  bool
}

type Delivery struct {
	ProtocolVersion string               `json:"protocol_version"`
	Grant           protocol.ActionGrant `json:"grant"`
	DeliveryToken   string               `json:"delivery_token"`
}

type acknowledgement struct {
	ProtocolVersion string `json:"protocol_version"`
	DeliveryToken   string `json:"delivery_token"`
}

type resultSubmission struct {
	ProtocolVersion string          `json:"protocol_version"`
	DeliveryToken   string          `json:"delivery_token"`
	Result          protocol.Result `json:"result"`
}

func (s *RunnerServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/runner/grants:next", s.next)
	mux.HandleFunc("POST /v1/runner/grants/{id}/ack", s.ack)
	mux.HandleFunc("POST /v1/runner/grants/{id}/result", s.result)
	mux.HandleFunc("POST /v1/runner/grants/{id}/reconcile", s.recovery)
	mux.HandleFunc("POST /v1/runner/heartbeat", s.heartbeat)
	return mux
}

func (s *RunnerServer) next(writer http.ResponseWriter, request *http.Request) {
	identity, ok := s.authenticate(writer, request)
	if !ok {
		return
	}
	wait := parseWait(request.URL.Query().Get("wait"), s.maxWait())
	deadline := s.now().Add(wait)
	for {
		allowed, err := s.dispatchAllowed(request.Context(), identity)
		if err != nil {
			writeRunnerError(writer, http.StatusServiceUnavailable, "runner fleet store unavailable")
			return
		}
		if !allowed {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		now := s.now()
		token, err := randomToken()
		if err != nil {
			writeRunnerError(writer, http.StatusInternalServerError, "create delivery token")
			return
		}
		record, err := s.Store.ClaimGrantDispatch(request.Context(), identity.TenantID, identity.RunnerGroup, identity.RunnerID, token, now, now.Add(s.lease()))
		if err == nil {
			allowed, fleetErr := s.dispatchAllowed(request.Context(), identity)
			if fleetErr != nil {
				writeRunnerError(writer, http.StatusServiceUnavailable, "runner fleet store unavailable")
				return
			}
			if !allowed {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			if err := s.appendAudit(domain.AuditEvent{ID: auditID(record.Grant.GrantID, now), CorrelationID: record.Grant.RunID, ActorType: "runner", ActorID: identity.RunnerID, Action: "grant.dispatch", ResourceType: "action_grant", ResourceID: record.Grant.GrantID, Result: "DELIVERED", Details: map[string]any{"runner_group": identity.RunnerGroup}, Timestamp: now}); err != nil {
				writeRunnerError(writer, http.StatusServiceUnavailable, "audit store unavailable")
				return
			}
			writeRunnerJSON(writer, http.StatusOK, Delivery{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, Grant: record.Grant, DeliveryToken: record.DeliveryToken})
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeRunnerError(writer, http.StatusServiceUnavailable, "grant store unavailable")
			return
		}
		if !now.Before(deadline) || wait == 0 {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		pause := s.pollInterval()
		if remaining := deadline.Sub(now); pause > remaining {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *RunnerServer) ack(writer http.ResponseWriter, request *http.Request) {
	identity, ok := s.authenticate(writer, request)
	if !ok {
		return
	}
	var body acknowledgement
	if !decodeRunnerJSON(writer, request, &body) {
		return
	}
	if body.ProtocolVersion != protocol.RunnerTransportVersionV1Alpha1 || body.DeliveryToken == "" {
		writeRunnerError(writer, http.StatusUnprocessableEntity, "invalid grant acknowledgement")
		return
	}
	now := s.now()
	record, err := s.Store.AcknowledgeGrantDispatch(request.Context(), request.PathValue("id"), identity.RunnerID, body.DeliveryToken, now, now.Add(s.lease()))
	if err != nil {
		writeGrantStoreError(writer, err)
		return
	}
	if err := s.appendAudit(domain.AuditEvent{ID: auditID(record.Grant.GrantID, now), CorrelationID: record.Grant.RunID, ActorType: "runner", ActorID: identity.RunnerID, Action: "grant.ack", ResourceType: "action_grant", ResourceID: record.Grant.GrantID, Result: "ACKED", Timestamp: now}); err != nil {
		writeRunnerError(writer, http.StatusServiceUnavailable, "audit store unavailable")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *RunnerServer) result(writer http.ResponseWriter, request *http.Request) {
	identity, ok := s.authenticate(writer, request)
	if !ok {
		return
	}
	var body resultSubmission
	if !decodeRunnerJSON(writer, request, &body) {
		return
	}
	if body.ProtocolVersion != protocol.RunnerTransportVersionV1Alpha1 || body.DeliveryToken == "" {
		writeRunnerError(writer, http.StatusUnprocessableEntity, "invalid runner result envelope")
		return
	}
	grantID := request.PathValue("id")
	if body.Result.ProtocolVersion != protocol.VersionV1Alpha1 || body.Result.GrantID != grantID || body.Result.CompletedAt.IsZero() || !validResultStatus(body.Result.Status) {
		writeRunnerError(writer, http.StatusUnprocessableEntity, "invalid runner result")
		return
	}
	now := s.now()
	audit := domain.AuditEvent{ID: auditID(grantID, now), CorrelationID: body.Result.RunID, ActorType: "runner", ActorID: identity.RunnerID, Action: "grant.result", ResourceType: "action_grant", ResourceID: grantID, Result: string(body.Result.Status), Details: map[string]any{"step_id": body.Result.StepID, "reason_code": body.Result.ReasonCode, "external_execution_id": body.Result.ExternalExecutionID}, Timestamp: now}
	if _, err := s.Store.CompleteGrantDispatch(request.Context(), grantID, identity.RunnerID, body.DeliveryToken, body.Result, now, audit); err != nil {
		writeGrantStoreError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *RunnerServer) heartbeat(writer http.ResponseWriter, request *http.Request) {
	identity, ok := s.authenticate(writer, request)
	if !ok {
		return
	}
	if s.Fleet == nil {
		writeRunnerError(writer, http.StatusServiceUnavailable, "runner fleet store unavailable")
		return
	}
	var body protocol.RunnerHeartbeat
	if !decodeRunnerJSON(writer, request, &body) {
		return
	}
	if body.ProtocolVersion != protocol.RunnerTransportVersionV1Alpha1 || body.ObservedAt.IsZero() || body.Capacity < 1 || body.InFlight < 0 || body.InFlight > body.Capacity {
		writeRunnerError(writer, http.StatusUnprocessableEntity, "invalid runner heartbeat")
		return
	}
	now := s.now()
	var audit domain.AuditEvent
	existing, existingErr := s.Fleet.GetRunner(request.Context(), identity.TenantID, identity.RunnerID)
	if errors.Is(existingErr, store.ErrNotFound) {
		audit = domain.AuditEvent{ID: auditID(identity.RunnerID, now), CorrelationID: "runner/" + identity.TenantID + "/" + identity.RunnerID, ActorType: "runner", ActorID: identity.RunnerID, Action: "runner.connect", ResourceType: "runner", ResourceID: identity.RunnerID, Result: "CONNECTED", Details: map[string]any{"runner_group": identity.RunnerGroup, "capacity": body.Capacity, "in_flight": body.InFlight}, Timestamp: now}
	} else if existingErr == nil && existing.LastSeen.Add(3*s.heartbeatInterval()).Before(now) {
		audit = domain.AuditEvent{ID: auditID(identity.RunnerID, now), CorrelationID: "runner/" + identity.TenantID + "/" + identity.RunnerID, ActorType: "runner", ActorID: identity.RunnerID, Action: "runner.reconnect", ResourceType: "runner", ResourceID: identity.RunnerID, Result: "RECONNECTED", Details: map[string]any{"runner_group": identity.RunnerGroup, "previous_last_seen": existing.LastSeen}, Timestamp: now}
	} else if existingErr != nil {
		writeGrantStoreError(writer, existingErr)
		return
	}
	registered, err := s.Fleet.HeartbeatRunner(request.Context(), domain.RunnerInfo{ID: identity.RunnerID, TenantID: identity.TenantID, Group: identity.RunnerGroup, ReportedCapacity: body.Capacity, InFlight: body.InFlight, LastSeen: now}, audit)
	if err != nil {
		writeGrantStoreError(writer, err)
		return
	}
	writeRunnerJSON(writer, http.StatusOK, protocol.RunnerControl{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, Mode: runnerMode(registered.Status), HeartbeatInterval: s.heartbeatInterval().String()})
}

func (s *RunnerServer) authenticate(writer http.ResponseWriter, request *http.Request) (RunnerIdentity, bool) {
	if s.Store == nil || s.Auth == nil {
		writeRunnerError(writer, http.StatusServiceUnavailable, "runner transport unavailable")
		return RunnerIdentity{}, false
	}
	identity, err := s.Auth.Authenticate(request)
	if err != nil {
		runnerID := strings.TrimSpace(request.Header.Get(runnerIDHeader))
		if runnerID == "" {
			runnerID = "unknown"
		}
		now := s.now()
		_ = s.appendAudit(domain.AuditEvent{ID: auditID(runnerID, now), CorrelationID: "runner-auth/" + runnerID, ActorType: "runner", ActorID: runnerID, Action: "runner.authenticate", ResourceType: "runner", ResourceID: runnerID, Result: "REJECTED", Details: map[string]any{"reason_code": "RUNNER_AUTHENTICATION_FAILED"}, Timestamp: now})
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeRunnerError(writer, http.StatusUnauthorized, "runner authentication failed")
		return RunnerIdentity{}, false
	}
	return identity, true
}

type GrantExecutor interface {
	Execute(context.Context, protocol.ActionGrant) (protocol.Result, error)
}

type RunnerClient struct {
	BaseURL           string
	RunnerID          string
	Token             string
	HTTP              *http.Client
	Wait              time.Duration
	Backoff           time.Duration
	Connectivity      *runner.AtomicConnectionState
	Runtime           *runner.RuntimeState
	Capacity          int
	HeartbeatInterval time.Duration
	BeforeExecute     func(context.Context, protocol.ActionGrant) error
}

func (c *RunnerClient) Run(ctx context.Context, executor GrantExecutor) error {
	if executor == nil {
		return errors.New("runner executor is required")
	}
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	if c.Runtime == nil {
		c.Runtime = runner.NewRuntimeState(c.capacity())
	}
	heartbeatContext, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go c.heartbeatLoop(heartbeatContext)
	for {
		if !c.Runtime.Accepting() {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				continue
			}
		}
		_, err := c.RunOnce(ctx, executor)
		if err == nil {
			continue
		}
		c.setConnected(false)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *RunnerClient) RunOnce(ctx context.Context, executor GrantExecutor) (bool, error) {
	delivery, found, err := c.poll(ctx)
	if err != nil || !found {
		return found, err
	}
	c.setConnected(true)
	if err := c.ack(ctx, delivery); err != nil {
		return true, err
	}
	if c.BeforeExecute != nil {
		if err := c.BeforeExecute(ctx, delivery.Grant); err != nil {
			result := protocol.Result{ProtocolVersion: protocol.VersionV1Alpha1, GrantID: delivery.Grant.GrantID, RunID: delivery.Grant.RunID, StepID: delivery.Grant.StepID, Status: protocol.ResultRejected, ReasonCode: preExecutionReason(err), CompletedAt: time.Now().UTC()}
			if submitErr := c.submitResult(ctx, delivery, result); submitErr != nil {
				return true, submitErr
			}
			return true, nil
		}
	}
	result, executeErr := executor.Execute(ctx, delivery.Grant)
	if err := c.submitResult(ctx, delivery, result); err != nil {
		return true, err
	}
	// Rejections and UNKNOWN are durable results, not transport failures. They
	// must be reported before the local execution error is discarded.
	_ = executeErr
	return true, nil
}

func (c *RunnerClient) poll(ctx context.Context) (Delivery, bool, error) {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/v1/runner/grants:next")
	if err != nil {
		return Delivery{}, false, err
	}
	wait := c.Wait
	if wait <= 0 {
		wait = defaultMaxWait
	}
	query := base.Query()
	query.Set("wait", wait.String())
	base.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return Delivery{}, false, err
	}
	c.authorize(request)
	response, err := c.http().Do(request)
	if err != nil {
		return Delivery{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		c.setConnected(true)
		return Delivery{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return Delivery{}, false, responseError(response)
	}
	var delivery Delivery
	if err := decodeResponse(response.Body, &delivery); err != nil {
		return Delivery{}, false, err
	}
	if delivery.ProtocolVersion != protocol.RunnerTransportVersionV1Alpha1 || delivery.DeliveryToken == "" || delivery.Grant.GrantID == "" {
		return Delivery{}, false, errors.New("invalid grant delivery")
	}
	return delivery, true, nil
}

func (c *RunnerClient) ack(ctx context.Context, delivery Delivery) error {
	return c.post(ctx, "/v1/runner/grants/"+url.PathEscape(delivery.Grant.GrantID)+"/ack", acknowledgement{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, DeliveryToken: delivery.DeliveryToken})
}

func (c *RunnerClient) submitResult(ctx context.Context, delivery Delivery, result protocol.Result) error {
	return c.post(ctx, "/v1/runner/grants/"+url.PathEscape(delivery.Grant.GrantID)+"/result", resultSubmission{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, DeliveryToken: delivery.DeliveryToken, Result: result})
}

func (c *RunnerClient) heartbeatLoop(ctx context.Context) {
	interval := c.heartbeatInterval()
	for {
		if err := c.heartbeat(ctx); err != nil {
			c.setConnected(false)
			c.Runtime.SetMode(protocol.RunnerModeFrozen)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *RunnerClient) heartbeat(ctx context.Context) error {
	if c.Runtime == nil {
		c.Runtime = runner.NewRuntimeState(c.capacity())
	}
	capacity, inFlight := c.Runtime.Snapshot()
	body := protocol.RunnerHeartbeat{ProtocolVersion: protocol.RunnerTransportVersionV1Alpha1, ObservedAt: time.Now().UTC(), Capacity: capacity, InFlight: inFlight}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/runner/heartbeat", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.http().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	var control protocol.RunnerControl
	if err := decodeResponse(response.Body, &control); err != nil {
		return err
	}
	if control.ProtocolVersion != protocol.RunnerTransportVersionV1Alpha1 || (control.Mode != protocol.RunnerModeActive && control.Mode != protocol.RunnerModeDraining && control.Mode != protocol.RunnerModeFrozen) {
		return errors.New("invalid runner control response")
	}
	c.Runtime.SetMode(control.Mode)
	c.setConnected(true)
	return nil
}

func (c *RunnerClient) post(ctx context.Context, path string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.http().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return responseError(response)
	}
	return nil
}

func (c *RunnerClient) authorize(request *http.Request) {
	request.Header.Set(runnerIDHeader, c.RunnerID)
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
}
func (c *RunnerClient) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}
func (c *RunnerClient) setConnected(value bool) {
	if c.Connectivity != nil {
		c.Connectivity.Set(value)
	}
}

func (c *RunnerClient) capacity() int {
	if c.Capacity > 0 {
		return c.Capacity
	}
	return 1
}

func (c *RunnerClient) heartbeatInterval() time.Duration {
	if c.HeartbeatInterval > 0 {
		return c.HeartbeatInterval
	}
	return 15 * time.Second
}

func decodeRunnerJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeRunnerError(writer, http.StatusBadRequest, "invalid JSON")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeRunnerError(writer, http.StatusBadRequest, "trailing JSON")
		return false
	}
	return true
}

func decodeResponse(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func responseError(response *http.Response) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("control plane returned %s: %s", response.Status, strings.TrimSpace(string(message)))
}

func writeGrantStoreError(writer http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeRunnerError(writer, http.StatusNotFound, "grant not found")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeRunnerError(writer, http.StatusConflict, "grant delivery conflict")
		return
	}
	writeRunnerError(writer, http.StatusServiceUnavailable, "grant store unavailable")
}

func writeRunnerError(writer http.ResponseWriter, status int, message string) {
	writeRunnerJSON(writer, status, map[string]string{"error": message})
}
func writeRunnerJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func validResultStatus(status protocol.ResultStatus) bool {
	return status == protocol.ResultSucceeded || status == protocol.ResultRejected || status == protocol.ResultUnknown || status == protocol.ResultReconciliation
}

func parseWait(value string, maximum time.Duration) time.Duration {
	if value == "" {
		return maximum
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return 0
	}
	if duration > maximum {
		return maximum
	}
	return duration
}
func (s *RunnerServer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *RunnerServer) maxWait() time.Duration {
	if s.MaxWait > 0 {
		return s.MaxWait
	}
	return defaultMaxWait
}
func (s *RunnerServer) lease() time.Duration {
	if s.Lease > 0 {
		return s.Lease
	}
	return defaultLease
}
func (s *RunnerServer) pollInterval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return 100 * time.Millisecond
}
func (s *RunnerServer) heartbeatInterval() time.Duration {
	if s.HeartbeatInterval > 0 {
		return s.HeartbeatInterval
	}
	return 15 * time.Second
}

func (s *RunnerServer) dispatchAllowed(ctx context.Context, identity RunnerIdentity) (bool, error) {
	if s.Fleet == nil {
		return !s.RequireHeartbeat, nil
	}
	registered, err := s.Fleet.GetRunner(ctx, identity.TenantID, identity.RunnerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return !s.RequireHeartbeat, nil
		}
		return false, err
	}
	return registered.Group == identity.RunnerGroup && registered.Status == domain.RunnerReady && registered.LastSeen.Add(3*s.heartbeatInterval()).After(s.now()), nil
}

func (s *RunnerServer) appendAudit(event domain.AuditEvent) error {
	if s.Audit == nil {
		return nil
	}
	return s.Audit.AppendAudit(event)
}

func runnerMode(status domain.RunnerStatus) protocol.RunnerMode {
	switch status {
	case domain.RunnerReady:
		return protocol.RunnerModeActive
	case domain.RunnerDraining:
		return protocol.RunnerModeDraining
	default:
		return protocol.RunnerModeFrozen
	}
}

func preExecutionReason(err error) string {
	switch {
	case errors.Is(err, grantvalidation.ErrInvalidProtocol):
		return "INVALID_PROTOCOL_VERSION"
	case errors.Is(err, grantvalidation.ErrInvalidIssuer):
		return "INVALID_GRANT_ISSUER"
	case errors.Is(err, grantvalidation.ErrWrongAudience):
		return "WRONG_RUNNER_AUDIENCE"
	case errors.Is(err, grantvalidation.ErrWrongTenant):
		return "WRONG_TENANT"
	case errors.Is(err, grantvalidation.ErrExpired):
		return "EXPIRED_GRANT"
	default:
		return "INVALID_GRANT_SIGNATURE"
	}
}
func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
func auditID(grantID string, now time.Time) string {
	value, err := randomToken()
	if err != nil {
		return fmt.Sprintf("grant-result/%s/%d", grantID, now.UnixNano())
	}
	return "grant-result/" + value
}
