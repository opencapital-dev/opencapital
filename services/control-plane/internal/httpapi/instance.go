package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/portfolio-management/control-plane/internal/install"
)

// v8 instance-bootstrap support. Each Grafana process (desktop or cloud
// container) runs `instance-bootstrap` before `grafana-server` to pull
// the org's installed plugins + their platform_tokens from control plane
// and render Grafana provisioning YAML. This endpoint is the read side
// of that flow.
//
// Auth: the operator AdminBootstrapToken (env-gated, constant-time
// compared). For v0 desktop + cloud all use the same operator-shared
// secret; Phase 3 introduces per-instance bootstrap credentials.

type instanceArtifact struct {
	DownloadURL string `json:"download_url"`
	Sha256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
}

type instancePluginEntry struct {
	PluginID       string                `json:"plugin_id"`
	GrafanaSlug    string                `json:"grafana_slug"`
	PlatformToken  string                `json:"platform_token"`
	Required       bool                  `json:"required"`
	Type           string                `json:"type"`
	PlatformPlugin bool                  `json:"platform_plugin,omitempty"`
	Version        string                `json:"version,omitempty"`
	QueryEntities  []install.QueryEntity `json:"query_entities,omitempty"`
	Artifact       *instanceArtifact     `json:"artifact,omitempty"`
}

// handleInstanceListPlugins returns every plugin install for the org
// alongside the manifest metadata instance-bootstrap needs to render
// provisioning YAML. Returns plugins whose manifest is unknown to the
// control plane as a soft skip (logged warn) rather than failing the
// whole bootstrap: a manifest drift on a single plugin shouldn't keep
// the instance from booting.
func (s *Server) handleInstanceListPlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBootstrapToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	orgIDStr := r.PathValue("org_id")
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		http.Error(w, "org_id not a UUID", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	installed, err := s.store.ListPluginInstallsForOrg(ctx, orgID)
	if err != nil {
		s.logger.Error("instance: list plugin installs", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	out := make([]instancePluginEntry, 0, len(installed))
	for _, p := range installed {
		// Skip rows mid-uninstall — the plugin's RW role + views are
		// about to be dropped, so renaming its YAML in is pointless and
		// would cause Grafana to keep retrying a doomed config.
		if p.UninstallState != "" {
			continue
		}

		rp, found, err := s.registry.Get(ctx, p.PluginID)
		if err != nil {
			s.logger.Warn("instance: registry lookup failed, skipping plugin",
				"org", orgID, "plugin", p.PluginID, "err", err)
			continue
		}
		if !found {
			s.logger.Warn("instance: plugin not in registry, skipping",
				"org", orgID, "plugin", p.PluginID)
			continue
		}

		entry := instancePluginEntry{
			PluginID:       p.PluginID,
			GrafanaSlug:    rp.GrafanaSlug,
			PlatformToken:  p.PlatformToken,
			Required:       rp.Required,
			Type:           rp.Type,
			PlatformPlugin: rp.PlatformPlugin,
			QueryEntities:  rp.QueryEntities,
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInstanceListVersions returns all known versions of a plugin with their
// validation status, for use by instance-bootstrap (bootstrap-token auth).
// Reuses the same response type + registry method as the Kinde-facing
// handleListPluginVersions — instance-bootstrap needs the validated flag to
// decide which version to pin on first install.
func (s *Server) handleInstanceListVersions(w http.ResponseWriter, r *http.Request) {
	if !s.requireBootstrapToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	versions, err := s.registry.VersionsWithStatus(r.Context(), id)
	if err != nil {
		s.logger.Error("instance: list versions", "err", err, "plugin_id", id)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, listPluginVersionsResponse{PluginID: id, Versions: versions})
}

// handleInstanceArtifact returns the artifact metadata (download URL, digest,
// size) for a specific (plugin, version, platform) from either the trusted or
// staging namespace. Bootstrap-token authenticated; used by instance-bootstrap
// to fetch preview artifacts during version resolution.
func (s *Server) handleInstanceArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireBootstrapToken(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	version := r.PathValue("version")
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		http.Error(w, "platform required", http.StatusBadRequest)
		return
	}
	art, ok, err := s.registry.ResolveArtifact(r.Context(), id, version, platform)
	if err != nil {
		s.logger.Error("instance: resolve artifact", "err", err, "plugin_id", id, "version", version)
		http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, instanceArtifact{
		DownloadURL: art.DownloadURL, Sha256: art.Sha256, SizeBytes: art.SizeBytes,
	})
}

// requireBootstrapToken does a constant-time compare of the request's
// bearer against the operator AdminBootstrapToken. v0: same secret as
// the /admin/* endpoints; per-instance bootstrap creds are a Phase 3
// follow-up.
func (s *Server) requireBootstrapToken(r *http.Request) bool {
	if s.cfg.AdminBootstrapToken == "" {
		return false
	}
	bearer, ok := extractBearer(r)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(bearer), []byte(s.cfg.AdminBootstrapToken)) == 1
}
