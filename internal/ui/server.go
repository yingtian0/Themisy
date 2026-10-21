package ui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"themisy/internal/application"
	webassets "themisy/web"
)

type Identity struct {
	Subject  string
	TenantID string
	Roles    []string
}

type IdentityVerifier interface {
	Verify(*http.Request) (Identity, error)
}

type HeaderIdentityVerifier struct{}

func (HeaderIdentityVerifier) Verify(r *http.Request) (Identity, error) {
	subject := strings.TrimSpace(r.Header.Get("X-Actor-ID"))
	tenant := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if subject == "" || tenant == "" {
		return Identity{}, errors.New("authenticated actor and tenant are required")
	}
	parts := strings.Split(r.Header.Get("X-Actor-Roles"), ",")
	roles := make([]string, 0, len(parts))
	for _, part := range parts {
		if role := strings.TrimSpace(part); role != "" {
			roles = append(roles, role)
		}
	}
	return Identity{Subject: subject, TenantID: tenant, Roles: roles}, nil
}

type Server struct {
	app      application.ControlPlane
	identity IdentityVerifier
	template *template.Template
	secret   []byte
	mux      *http.ServeMux
	now      func() time.Time
}

func New(app application.ControlPlane, identity IdentityVerifier) (*Server, error) {
	views, err := template.ParseFS(webassets.Assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	s := &Server{app: app, identity: identity, template: views, secret: secret, mux: http.NewServeMux()}
	s.now = time.Now
	s.mux.HandleFunc("GET /ui/approvals", s.approvals)
	s.mux.HandleFunc("POST /ui/approvals/{operation}", s.decide)
	s.mux.HandleFunc("GET /ui/release-runs/{id}", s.run)
	s.mux.Handle("GET /ui/static/", http.StripPrefix("/ui/", http.FileServerFS(webassets.Assets)))
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.verified(w, r)
	if !ok {
		return
	}
	approvals, err := s.app.ListPendingApprovals(identity.TenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	token := s.csrfToken(identity)
	cards := make([]approvalCard, 0, len(approvals))
	for _, summary := range approvals {
		run, err := s.app.GetForTenant(identity.TenantID, summary.RunID)
		if err != nil {
			run = nil
		}
		cards = append(cards, makeApprovalCard(summary, run, s.now()))
	}
	http.SetCookie(w, &http.Cookie{Name: "themisy_csrf", Value: token, Path: "/ui/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
	if err := s.template.ExecuteTemplate(w, "approval", approvalView{Title: "Approvals", Approvals: cards, CSRFToken: token}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.verified(w, r)
	if !ok {
		return
	}
	run, err := s.app.GetForTenant(identity.TenantID, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	events, err := s.app.EventsForTenant(identity.TenantID, run.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.template.ExecuteTemplate(w, "run", runView{Title: "Release " + run.ID, Run: run, Events: events}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.verified(w, r)
	if !ok {
		return
	}
	id, action, ok := splitOperation(r.PathValue("operation"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil || !s.validCSRF(r, identity) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	approvals, err := s.app.ListPendingApprovals(identity.TenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runID := ""
	for _, approval := range approvals {
		if approval.Approval.ID == id {
			if action == "approve" {
				run, err := s.app.GetForTenant(identity.TenantID, approval.RunID)
				if err != nil {
					http.NotFound(w, r)
					return
				}
				card := makeApprovalCard(approval, run, s.now())
				if !card.Reviewable || r.FormValue("plan_hash") != card.Approval.PlanHash {
					http.Error(w, "approval display is expired or no longer matches its pinned Plan; refresh and reauthorize", http.StatusConflict)
					return
				}
			}
			runID = approval.RunID
			break
		}
	}
	if runID == "" {
		http.NotFound(w, r)
		return
	}
	if action == "revoke" {
		_, err = s.app.RevokeApprovalForTenant(r.Context(), identity.TenantID, runID, id, identity.Subject, identity.Roles)
	} else {
		_, err = s.app.DecideApprovalForTenant(r.Context(), identity.TenantID, runID, id, identity.Subject, identity.Roles, action == "approve")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/ui/approvals", http.StatusSeeOther)
}

func (s *Server) verified(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	identity, err := s.identity.Verify(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return Identity{}, false
	}
	if identity.Subject == "" || identity.TenantID == "" {
		http.Error(w, "identity verifier returned an incomplete identity", http.StatusUnauthorized)
		return Identity{}, false
	}
	return identity, true
}

func (s *Server) csrfToken(identity Identity) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(identity.Subject + "\x00" + identity.TenantID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validCSRF(r *http.Request, identity Identity) bool {
	cookie, err := r.Cookie("themisy_csrf")
	if err != nil {
		return false
	}
	want := s.csrfToken(identity)
	return hmac.Equal([]byte(cookie.Value), []byte(want)) && hmac.Equal([]byte(r.FormValue("csrf_token")), []byte(want))
}

func splitOperation(value string) (string, string, bool) {
	id, action, ok := strings.Cut(value, ":")
	if !ok || id == "" || (action != "approve" && action != "deny" && action != "revoke") {
		return "", "", false
	}
	return id, action, true
}
