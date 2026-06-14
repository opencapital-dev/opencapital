package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	grafanaclient "github.com/portfolio-management/control-plane/internal/grafana"
	"github.com/portfolio-management/control-plane/internal/store"
)

// Phase 7 landing/onboarding surface. The browser comes through Caddy
// with a Grafana session cookie attached; control-plane validates that
// cookie by server-side calling Grafana's /api/user, then exposes
// /api/me (read membership) and /api/onboarding/orgs (create org +
// Grafana org + per-org plugin install).
//
// These routes are NOT guarded by the admin-bootstrap token. They live
// behind the Grafana session, which is itself behind Kinde OIDC. The
// user identity is whichever account the Grafana session belongs to;
// the user's control_db row is lazily provisioned on first sight.

// callerSession holds whatever the Grafana-session middleware
// resolved. Stored on request context so handlers don't re-fetch.
type callerSession struct {
	GrafanaUserID int64
	GrafanaLogin  string
	GrafanaEmail  string
	UserID        string // canonical control_db user_id (may equal email)
}

type sessionCtxKey struct{}

// orgNameRegex constrains org names so the value going into Grafana's
// /api/orgs payload (and to the postgres `name` column) can't carry
// control characters or stupidly long strings. Wide enough for unicode
// letters by allowing any non-control non-newline rune up to 64 chars.
var orgNameRegex = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} _.-]{1,63}$`)

// currencyRegex: ISO 4217-ish, three uppercase letters. The wizard
// only offers a small dropdown so we don't need to validate against a
// full enum here.
var currencyRegex = regexp.MustCompile(`^[A-Z]{3}$`)

type meResponse struct {
	UserID  string            `json:"user_id"`
	Login   string            `json:"login"`
	Email   string            `json:"email"`
	HasOrg  bool              `json:"has_org"`
	Orgs    []meResponseOrg   `json:"orgs"`
	Targets meResponseTargets `json:"targets"`
}

type meResponseOrg struct {
	OrgID    string `json:"org_id"`
	ShortID  string `json:"short_id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Currency string `json:"base_currency"`
}

type meResponseTargets struct {
	Grafana string `json:"grafana"` // URL to redirect to once an org exists
}

type createOrgRequest struct {
	Name         string `json:"name"`
	BaseCurrency string `json:"base_currency"`
}

type createOrgResponse struct {
	OrgID            string   `json:"org_id"`
	ShortID          string   `json:"short_id"`
	Name             string   `json:"name"`
	BaseCurrency     string   `json:"base_currency"`
	RedirectTo       string   `json:"redirect_to"`
	PluginsInstalled []string `json:"plugins_installed"`
}

// handleMe returns the caller's user_id + every org they belong to.
// Middleware guarantees the session has been validated; on a session
// failure middleware returns 401 before this handler runs.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if s.grafanaAdmin == nil {
		http.Error(w, "onboarding disabled", http.StatusServiceUnavailable)
		return
	}
	session, ok := sessionFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	memberships, err := s.store.ListOrgMembershipsForUser(ctx, session.UserID)
	if err != nil {
		s.logger.Error("me: list memberships", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	resp := meResponse{
		UserID: session.UserID,
		Login:  session.GrafanaLogin,
		Email:  session.GrafanaEmail,
		HasOrg: len(memberships) > 0,
		Orgs:   make([]meResponseOrg, 0, len(memberships)),
		Targets: meResponseTargets{
			Grafana: s.cfg.GrafanaPluginRedirectTo,
		},
	}
	for _, m := range memberships {
		resp.Orgs = append(resp.Orgs, meResponseOrg{
			OrgID:    m.OrgID.String(),
			ShortID:  m.ShortID,
			Name:     m.Name,
			Role:     m.Role,
			Currency: m.Currency,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCreateOrg builds a brand-new tenant under v8. Steps:
//  1. INSERT organisations (name, base_currency) -> org_id
//  2. INSERT user_org (caller_user_id, org_id, role='admin')
//  3. install.Install for each Required plugin -> per-(plugin, org) RW
//     schema + role + views + platform_token
//
// The Grafana org creation step from v6 is gone — every Grafana process
// under v8 is single-(deployment, org), so there is no "create another
// Grafana org" operation. Plugin platform_tokens land in the Grafana
// container via instance-bootstrap at next instance start (operator-
// restart in v0; self-service restart in Phase 3/4).
func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		http.Error(w, "onboarding disabled", http.StatusServiceUnavailable)
		return
	}
	session, ok := sessionFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body createOrgRequest
	if err := decodeStrict(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	currency := strings.ToUpper(strings.TrimSpace(body.BaseCurrency))
	if currency == "" {
		currency = "USD"
	}
	if !orgNameRegex.MatchString(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	if !currencyRegex.MatchString(currency) {
		http.Error(w, "bad currency", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Refuse if the user already has an org. Wizard is one-shot per user.
	existing, err := s.store.ListOrgMembershipsForUser(ctx, session.UserID)
	if err != nil {
		s.logger.Error("onboarding: list memberships", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if len(existing) > 0 {
		http.Error(w, "user already has an org", http.StatusConflict)
		return
	}

	orgID, shortID, err := s.store.CreateOrg(ctx, name, currency)
	if err != nil {
		s.logger.Error("onboarding: create control_db org", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if err := s.store.AddUserToOrg(ctx, session.UserID, orgID, "admin"); err != nil {
		s.logger.Error("onboarding: add user_org", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	// Install only the required plugins, resolving each directly (latest
	// trusted version, else the staging fallback). We deliberately do NOT build
	// the full marketplace catalog here: that would fetch every plugin in the
	// manifest — including non-required ones — and a single unreachable plugin
	// would abort onboarding. A required plugin that is unavailable in BOTH
	// namespaces is logged and skipped rather than failing org creation; only a
	// genuine registry/transport error (not a missing repo) is fatal.
	var installed []string
	for _, id := range s.registry.RequiredIDs() {
		rp, found, err := s.registry.Get(ctx, id)
		if err != nil {
			s.logger.Error("onboarding: resolve required plugin", "err", err, "plugin", id)
			http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
			return
		}
		if !found {
			s.logger.Warn("onboarding: required plugin unavailable, skipping", "plugin", id)
			continue
		}
		if _, err := s.installer.Install(ctx, orgID, shortID, rp.Footprint); err != nil {
			s.logger.Error("onboarding: plugin install", "err", err, "plugin", id, "org_id", orgID)
			http.Error(w, "plugin install failed", http.StatusInternalServerError)
			return
		}
		installed = append(installed, id)
	}

	s.audit(ctx, store.AuditEntry{
		Actor:       session.UserID,
		ActorSource: "grafana",
		Action:      "onboarding.org.create",
		Target: map[string]any{
			"org_id":   orgID.String(),
			"short_id": shortID,
			"name":     name,
		},
		Result:    "ok",
		RequestIP: requestIP(r),
	})

	writeJSON(w, http.StatusCreated, createOrgResponse{
		OrgID:            orgID.String(),
		ShortID:          shortID,
		Name:             name,
		BaseCurrency:     currency,
		RedirectTo:       s.cfg.GrafanaPluginRedirectTo,
		PluginsInstalled: installed,
	})
}

// requireGrafanaSession is a wrapper that validates the Grafana
// session cookie against grafana:3000/api/user. On success it stashes
// the resolved callerSession on the request context. On failure it
// writes 401 and returns.
func (s *Server) requireGrafanaSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.grafanaAdmin == nil {
			http.Error(w, "onboarding disabled", http.StatusServiceUnavailable)
			return
		}
		cookie, err := r.Cookie(s.cfg.GrafanaSessionCookie)
		if err != nil || cookie == nil || cookie.Value == "" {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		gUser, err := s.grafanaAdmin.GetUserBySession(ctx, cookie)
		if err != nil {
			if errors.Is(err, grafanaclient.ErrUnauthorized) {
				http.Error(w, "session rejected by grafana", http.StatusUnauthorized)
				return
			}
			s.logger.Error("onboarding: session validate", "err", err)
			http.Error(w, "grafana unreachable", http.StatusBadGateway)
			return
		}

		// Resolve / lazily create the control_db user. external_id for
		// Grafana sessions is "user:<integer-id>", matching the JWT
		// path's convention. Canonical user_id defaults to the email
		// (or login when email is empty) on first sight.
		externalID := grafanaExternalID(gUser.ID)
		userIDProposed := gUser.Email
		if userIDProposed == "" {
			userIDProposed = gUser.Login
		}
		userID, err := s.store.EnsureUserExternalID(ctx, "grafana", externalID, userIDProposed, "onboarding")
		if err != nil {
			s.logger.Error("onboarding: ensure user_external_ids", "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}

		session := callerSession{
			GrafanaUserID: gUser.ID,
			GrafanaLogin:  gUser.Login,
			GrafanaEmail:  gUser.Email,
			UserID:        userID,
		}
		ctx = context.WithValue(r.Context(), sessionCtxKey{}, session)
		next(w, r.WithContext(ctx))
	}
}

func sessionFromContext(ctx context.Context) (callerSession, bool) {
	v, ok := ctx.Value(sessionCtxKey{}).(callerSession)
	return v, ok
}

// grafanaExternalID renders a Grafana integer user id as the
// "user:<N>" handle stored in user_external_ids.external_id (same
// convention as the admin/users-link endpoint).
func grafanaExternalID(n int64) string { return "user:" + strconv.FormatInt(n, 10) }
