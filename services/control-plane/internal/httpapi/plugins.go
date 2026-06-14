package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/portfolio-management/control-plane/internal/registry"
	"github.com/portfolio-management/control-plane/internal/store"
)

// v6 Phase 8 self-service plugin catalog. All endpoints are mounted
// under /api/orgs/{org_id}/plugins and gated by
// requireGrafanaSession + an explicit "caller is admin for this org"
// check (requireOrgAdmin). Bootstrap-token callers don't reach these
// routes; they keep using the original POST /orgs/.../plugins/...
// operator break-glass path.
//
// See ADR-0050 for the design.

type catalogEntry struct {
	PluginID    string              `json:"plugin_id"`
	DisplayName string              `json:"display_name"`
	Description string              `json:"description"`
	Required    bool                `json:"required"`
	Installed   bool                `json:"installed"`
	InstalledAt string              `json:"installed_at,omitempty"`
	// UninstallState surfaces an in-flight uninstall so the UI can
	// disable controls until the worker finishes. Empty when the
	// plugin isn't being uninstalled.
	UninstallState     string              `json:"uninstall_state,omitempty"`
	UninstallKeysDone  int                 `json:"uninstall_keys_done,omitempty"`
	UninstallKeysTotal int                 `json:"uninstall_keys_total,omitempty"`
	Source             registry.SourceInfo `json:"source"`
}

type pluginInstallResponse struct {
	PluginID      string `json:"plugin_id"`
	OrgID         string `json:"org_id"`
	PlatformToken string `json:"platform_token"`
}

type uninstallStatus struct {
	PluginID  string `json:"plugin_id"`
	OrgID     string `json:"org_id"`
	State     string `json:"state"` // in_progress | failed | not_found
	KeysTotal *int   `json:"keys_total,omitempty"`
	KeysDone  int    `json:"keys_done"`
	LastError string `json:"last_error,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// requireOrgAdmin wraps requireGrafanaSession with a role check. The
// path's {org_id} is the source of truth (path-explicit, per ADR-0050).
// 401 if the caller isn't signed in; 403 if signed in but not an admin
// for the named org.
func (s *Server) requireOrgAdmin(next func(w http.ResponseWriter, r *http.Request, orgID uuid.UUID)) http.HandlerFunc {
	inner := func(w http.ResponseWriter, r *http.Request) {
		session, ok := sessionFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		orgIDStr := r.PathValue("org_id")
		orgID, err := uuid.Parse(orgIDStr)
		if err != nil {
			http.Error(w, "bad org_id", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		role, found, err := s.store.RoleForUserOrg(ctx, session.UserID, orgID)
		if err != nil {
			s.logger.Error("plugins: role lookup", "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		if !found || role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r, orgID)
	}
	return s.requireGrafanaSession(inner)
}

// handleListPlugins returns the catalog for an org. Joins
// install.DefaultManifests (the static registry) against
// plugin_installs rows so the UI sees every available plugin and
// whether it's currently installed.
func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request, orgID uuid.UUID) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	installed, err := s.store.ListPluginInstallsForOrg(ctx, orgID)
	if err != nil {
		s.logger.Error("plugins: list installs", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	idx := make(map[string]store.PluginInstall, len(installed))
	for _, p := range installed {
		idx[p.PluginID] = p
	}
	plugins, err := s.registry.List(ctx)
	if err != nil {
		s.logger.Error("plugins: registry list", "err", err)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	out := make([]catalogEntry, 0, len(plugins))
	for _, rp := range plugins {
		entry := catalogEntry{
			PluginID:    rp.PluginID,
			DisplayName: rp.DisplayName,
			Description: rp.Description,
			Required:    rp.Required,
		}
		entry.Source = rp.Source
		if p, ok := idx[rp.PluginID]; ok {
			entry.Installed = true
			entry.InstalledAt = p.GrantedAt.Format(time.RFC3339)
			entry.UninstallState = p.UninstallState
			entry.UninstallKeysDone = p.UninstallDone
			if p.UninstallTotal != nil {
				entry.UninstallKeysTotal = *p.UninstallTotal
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		// Required first, then alpha. Catalog UI renders top-to-bottom.
		if out[i].Required != out[j].Required {
			return out[i].Required
		}
		return out[i].PluginID < out[j].PluginID
	})
	writeJSON(w, http.StatusOK, out)
}

// resolvePluginVersion resolves the (footprint, version) an install should
// pin. An empty or "latest" version tracks the latest promoted version
// (registry.Get); any other value pins that exact promoted tag
// (registry.GetVersion). Writes the error response + returns ok=false on a
// registry error (503), an unknown plugin (404), or a requested version that
// isn't promoted (404). Shared by the install and re-pin handlers so version
// resolution lives in one place.
func (s *Server) resolvePluginVersion(w http.ResponseWriter, r *http.Request, ctx context.Context, pluginID, version string) (registry.Plugin, bool) {
	if version == "" || version == "latest" {
		rp, found, err := s.registry.Get(ctx, pluginID)
		if err != nil {
			s.logger.Error("plugins: registry lookup", "err", err, "plugin", pluginID)
			http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
			return registry.Plugin{}, false
		}
		if !found {
			http.Error(w, "unknown plugin", http.StatusNotFound)
			return registry.Plugin{}, false
		}
		return rp, true
	}
	rp, found, err := s.registry.GetVersion(ctx, pluginID, version)
	if err != nil {
		s.logger.Error("plugins: registry version lookup", "err", err, "plugin", pluginID, "version", version)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return registry.Plugin{}, false
	}
	if !found {
		http.Error(w, "version not found", http.StatusNotFound)
		return registry.Plugin{}, false
	}
	return rp, true
}

// handleInstallPlugin runs install.Install for the given plugin, then
// pushes per-(grafana_org) AppPluginConfig to Grafana so the plugin
// has its platform_token + jsonData in place for the calling user's
// org. Idempotent — re-install is safe.
func (s *Server) handleInstallPlugin(w http.ResponseWriter, r *http.Request, orgID uuid.UUID) {
	if s.installer == nil {
		http.Error(w, "install disabled", http.StatusServiceUnavailable)
		return
	}
	pluginID := r.PathValue("plugin_id")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	rp, ok := s.resolvePluginVersion(w, r, ctx, pluginID, "")
	if !ok {
		return
	}

	shortID, err := s.store.GetOrgShortID(ctx, orgID)
	if err != nil {
		s.logger.Error("plugins: short_id lookup", "err", err, "org_id", orgID)
		http.Error(w, "org not found", http.StatusNotFound)
		return
	}
	res, err := s.installer.Install(ctx, orgID, shortID, rp.Footprint)
	if err != nil {
		s.logger.Error("plugins: install", "err", err, "plugin", pluginID, "org", orgID)
		http.Error(w, "install failed", http.StatusInternalServerError)
		return
	}
	// v8: install creates the plugin_installs row + SQLite scaffold.
	// The provisioning YAML that hands the platform_token to the plugin
	// inside Grafana is rendered at instance start by instance-bootstrap;
	// a Grafana restart is required for the new plugin to become
	// visible to that instance. v0 leaves the restart to the operator
	// (or the desktop shell / cloud portal once Phase 3/4 land).
	s.audit(ctx, store.AuditEntry{
		Actor:       contextSession(r.Context()).UserID,
		ActorSource: "grafana",
		Action:      "plugins.install",
		Target: map[string]any{
			"org_id":    orgID.String(),
			"plugin_id": pluginID,
		},
		Result:    "ok",
		RequestIP: requestIP(r),
	})
	writeJSON(w, http.StatusCreated, pluginInstallResponse{
		PluginID:      pluginID,
		OrgID:         orgID.String(),
		PlatformToken: res.PlatformToken,
	})
}

// handleUninstallPlugin returns 202 and launches the async worker.
// The actual destructive work runs in s.runUninstall (plugins_uninstall.go).
// Required plugins are refused with 409.
func (s *Server) handleUninstallPlugin(w http.ResponseWriter, r *http.Request, orgID uuid.UUID) {
	if s.installer == nil || s.gatewayTombstone == nil {
		http.Error(w, "uninstall disabled", http.StatusServiceUnavailable)
		return
	}
	pluginID := r.PathValue("plugin_id")
	// Required is control-plane policy (checked without the registry, so an
	// un-published plugin can still be uninstalled). "Unknown" is covered by
	// the HasPluginInstall check below — an id with no install is just 404.
	if s.registry.IsRequired(pluginID) {
		http.Error(w, "plugin is required; cannot uninstall", http.StatusConflict)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	installed, err := s.store.HasPluginInstall(ctx, orgID, pluginID)
	if err != nil {
		s.logger.Error("plugins: install probe", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !installed {
		http.Error(w, "plugin not installed for this org", http.StatusNotFound)
		return
	}
	if err := s.store.MarkUninstallStarted(ctx, orgID, pluginID); err != nil {
		s.logger.Error("plugins: mark started", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	caller := contextSession(r.Context()).UserID
	s.audit(ctx, store.AuditEntry{
		Actor:       caller,
		ActorSource: "grafana",
		Action:      "plugins.uninstall.started",
		Target: map[string]any{
			"org_id":    orgID.String(),
			"plugin_id": pluginID,
		},
		Result:    "accepted",
		RequestIP: requestIP(r),
	})

	// Detach from the request context — the worker outlives the HTTP
	// response. Use a fresh context with a generous timeout; the
	// worker itself paginates so this is mostly a guard against a
	// genuinely stuck call.
	go s.runUninstall(context.Background(), orgID, pluginID, caller)

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"plugin_id": pluginID,
		"org_id":    orgID.String(),
		"state":     "in_progress",
	})
}

// handleUninstallStatus polls the row for the UI's progress bar.
// Returns 404 once the row has been deleted (uninstall completed).
func (s *Server) handleUninstallStatus(w http.ResponseWriter, r *http.Request, orgID uuid.UUID) {
	pluginID := r.PathValue("plugin_id")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	all, err := s.store.ListPluginInstallsForOrg(ctx, orgID)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	for _, p := range all {
		if p.PluginID != pluginID {
			continue
		}
		state := p.UninstallState
		if state == "" {
			state = "installed"
		}
		out := uninstallStatus{
			PluginID:  p.PluginID,
			OrgID:     p.OrgID.String(),
			State:     state,
			KeysTotal: p.UninstallTotal,
			KeysDone:  p.UninstallDone,
			LastError: p.UninstallError,
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusOK, uninstallStatus{
		PluginID: pluginID,
		OrgID:    orgID.String(),
		State:    "not_installed",
	})
}

// contextSession is a panic-safe wrapper around sessionFromContext for
// audit fields. Falls back to "" when no session is present (the
// middleware would have rejected the request before reaching here, so
// this is purely defensive).
func contextSession(ctx context.Context) callerSession {
	if v, ok := sessionFromContext(ctx); ok {
		return v
	}
	return callerSession{}
}

// errUninstallNotInProgress is what the resume path returns when a
// row's state changes out from under it.
var errUninstallNotInProgress = errors.New("uninstall row no longer in_progress")
