package httpapi

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/portfolio-management/control-plane/internal/registry"
)

// v8 shell-facing endpoints. Authenticated by the Kinde access-token
// middleware (requireKindeSession). Used by the desktop Tauri shell and
// the cloud portal before either ever boots a Grafana subprocess.

// --- GET /v1/me/orgs --------------------------------------------------------

type meOrgsEntry struct {
	OrgID    string `json:"org_id"`
	ShortID  string `json:"short_id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Currency string `json:"base_currency"`
}

type meOrgsResponse struct {
	UserID string        `json:"user_id"`
	Email  string        `json:"email"`
	Orgs   []meOrgsEntry `json:"orgs"`
}

func (s *Server) handleV1MeOrgs(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	memberships, err := s.store.ListOrgMembershipsForUser(ctx, session.UserID)
	if err != nil {
		s.logger.Error("v1/me/orgs: list memberships", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	out := meOrgsResponse{
		UserID: session.UserID,
		Email:  session.GrafanaEmail,
		Orgs:   make([]meOrgsEntry, 0, len(memberships)),
	}
	for _, m := range memberships {
		out.Orgs = append(out.Orgs, meOrgsEntry{
			OrgID:    m.OrgID.String(),
			ShortID:  m.ShortID,
			Name:     m.Name,
			Role:     m.Role,
			Currency: m.Currency,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- POST /v1/instance/token ------------------------------------------------

// The desktop shell (Kinde-authed) exchanges its Kinde access token for a
// short-lived instance token scoped to one org. The shell hands this token
// to plugins, which present it to /jwt/mint. This is the v8 identity model
// Option A: Kinde is the root authority (verified here against Kinde's
// public JWKS), and control-plane re-mints a token verifiable against its
// own JWKS — so the plugin->control-plane hop never depends on a roaming
// Grafana's unreachable JWKS, and plugins never hold the human's Kinde token.

type instanceTokenRequest struct {
	OrgID string `json:"org_id"`
}

type instanceTokenResponse struct {
	Token string `json:"token"`
	Exp   int64  `json:"exp"`
	OrgID string `json:"org_id"`
}

func (s *Server) handleV1InstanceToken(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body instanceTokenRequest
	if err := decodeStrict(r, &body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	orgID, err := uuid.Parse(body.OrgID)
	if err != nil {
		http.Error(w, "org_id not a UUID", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	member, err := s.store.HasUserOrg(ctx, session.UserID, orgID)
	if err != nil {
		s.logger.Error("v1/instance/token: user_org check", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !member {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tok, exp, err := s.signInstance(session.UserID, orgID.String())
	if err != nil {
		s.logger.Error("v1/instance/token: sign", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, instanceTokenResponse{Token: tok, Exp: exp, OrgID: orgID.String()})
}

// --- GET /v1/marketplace/catalog?org_id=X -----------------------------------

type marketplaceEntry struct {
	PluginID               string              `json:"plugin_id"`
	GrafanaSlug            string              `json:"grafana_slug"`
	DisplayName            string              `json:"display_name"`
	Description            string              `json:"description"`
	Type                   string              `json:"type"`
	Required               bool                `json:"required"`
	Installed              bool                `json:"installed"`
	InstalledAt            string              `json:"installed_at,omitempty"`
	LatestValidatedVersion string              `json:"latest_validated_version,omitempty"`
	Source                 registry.SourceInfo `json:"source"`
}

type marketplaceCatalogResponse struct {
	OrgID   string             `json:"org_id"`
	Plugins []marketplaceEntry `json:"plugins"`
}

func (s *Server) handleV1MarketplaceCatalog(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	orgIDStr := r.URL.Query().Get("org_id")
	if orgIDStr == "" {
		http.Error(w, "org_id query param required", http.StatusBadRequest)
		return
	}
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		http.Error(w, "org_id not a UUID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Entitlement gate: caller must be a member of the org they're
	// asking about. Role doesn't matter — read-only catalog. Refuse if
	// not a member so we don't leak plugin entitlements across orgs.
	if member, err := s.store.HasUserOrg(ctx, session.UserID, orgID); err != nil {
		s.logger.Error("v1/marketplace: user_org check", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	} else if !member {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	grants, err := s.store.ListPluginInstallsForOrg(ctx, orgID)
	if err != nil {
		s.logger.Error("v1/marketplace: list installs", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	type grantInfo struct {
		grantedAt time.Time
	}
	grantedByID := make(map[string]grantInfo, len(grants))
	for _, p := range grants {
		// Treat mid-uninstall rows as not installed so the UI lets the
		// user re-install once the worker completes.
		if p.UninstallState == "" {
			grantedByID[p.PluginID] = grantInfo{grantedAt: p.GrantedAt}
		}
	}

	plugins, err := s.registry.List(ctx)
	if err != nil {
		s.logger.Error("v1/marketplace: registry list", "err", err)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	out := marketplaceCatalogResponse{
		OrgID:   orgID.String(),
		Plugins: make([]marketplaceEntry, 0, len(plugins)),
	}
	for _, rp := range plugins {
		entry := marketplaceEntry{
			PluginID:               rp.PluginID,
			GrafanaSlug:            rp.GrafanaSlug,
			DisplayName:            rp.DisplayName,
			Description:            rp.Description,
			Type:                   rp.Type,
			Required:               rp.Required,
			LatestValidatedVersion: rp.Version,
		}
		entry.Source = rp.Source
		if info, ok := grantedByID[rp.PluginID]; ok {
			entry.Installed = true
			entry.InstalledAt = info.grantedAt.Format(time.RFC3339)
		}
		out.Plugins = append(out.Plugins, entry)
	}
	// Required plugins first, then alpha. Same ordering as the existing
	// /api/orgs/{org_id}/plugins endpoint so the two surfaces stay in
	// sync visually.
	sort.Slice(out.Plugins, func(i, j int) bool {
		if out.Plugins[i].Required != out.Plugins[j].Required {
			return out.Plugins[i].Required
		}
		return out.Plugins[i].PluginID < out.Plugins[j].PluginID
	})
	writeJSON(w, http.StatusOK, out)
}

// --- POST/DELETE /v1/orgs/{org_id}/plugins/{plugin_id} ----------------------

// Kinde-authed install/uninstall for the desktop marketplace. The shell
// drives these with the logged-in user's Kinde token; both verify org
// membership, then delegate to the existing install/uninstall handlers
// (same idempotent install + async uninstall worker the Grafana-admin
// surface uses). The reconciler picks up the changed set on next launch.

func (s *Server) handleV1InstallPlugin(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.kindeOrgMember(w, r)
	if !ok {
		return
	}
	s.handleInstallPlugin(w, r, orgID)
}

func (s *Server) handleV1UninstallPlugin(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.kindeOrgMember(w, r)
	if !ok {
		return
	}
	s.handleUninstallPlugin(w, r, orgID)
}

// --- GET /v1/marketplace/plugins/{id}/versions --------------------------------

type listPluginVersionsResponse struct {
	PluginID string                   `json:"plugin_id"`
	Versions []registry.VersionStatus `json:"versions"`
}

// handleListPluginVersions returns all known versions of a plugin with their
// validation status (validated=true means promoted/trusted; false means preview).
// Kinde-authenticated; any org member may read the version list.
func (s *Server) handleListPluginVersions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	versions, err := s.registry.VersionsWithStatus(r.Context(), id)
	if err != nil {
		s.logger.Error("v1/marketplace: list versions", "err", err, "plugin_id", id)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	if versions == nil {
		versions = []registry.VersionStatus{}
	}
	writeJSON(w, http.StatusOK, listPluginVersionsResponse{PluginID: id, Versions: versions})
}

// kindeOrgMember resolves the {org_id} path value and confirms the Kinde
// caller is a member of it. Writes the error response + returns ok=false on
// any failure. Desktop is single-user (the owner is the org admin), so
// membership is the gate; finer role checks are a follow-up.
func (s *Server) kindeOrgMember(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	session, ok := sessionFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return uuid.Nil, false
	}
	orgID, err := uuid.Parse(r.PathValue("org_id"))
	if err != nil {
		http.Error(w, "org_id not a UUID", http.StatusBadRequest)
		return uuid.Nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	member, err := s.store.HasUserOrg(ctx, session.UserID, orgID)
	if err != nil {
		s.logger.Error("v1 plugins: user_org check", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return uuid.Nil, false
	}
	if !member {
		http.Error(w, "forbidden", http.StatusForbidden)
		return uuid.Nil, false
	}
	return orgID, true
}
