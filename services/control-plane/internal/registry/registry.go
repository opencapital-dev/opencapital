// Package registry reads the v8 plugin catalog from an OCI registry (GHCR).
// Each plugin is published as an OCI artifact whose config blob is the
// plugin's footprint config blob (its install Footprint, derived from plugin.json) and
// whose layers are per-platform tarballs, each annotated with its os-arch.
// This replaces the old hardcoded install.DefaultManifests map: publishing
// a plugin is now an `oras push`, and control-plane reads everything —
// footprint, version, per-platform artifact digest — from the registry at
// runtime.
//
// The package splits into two clients: a federated catalog Client
// (catalog.go) that resolves plugins across a PluginProvider with NO registry
// coords of its own, and a StagingClient (staging.go) the janitor uses to
// prune/promote-check OpenCapital's own staging namespace.
//
// `required` is intentionally NOT read from the plugin: whether a plugin is
// mandatory for every workspace is control-plane policy, not something a
// plugin may self-declare. The Client applies it from a code-side set.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/portfolio-management/control-plane/internal/install"
)

// platformAnnotation must match the annotation plugindist stamps on each
// per-platform tarball layer.
const platformAnnotation = "io.opencapital.platform"

// Plugin is a catalog entry: the plugin's self-described footprint plus
// control-plane-applied policy (Required), the resolved latest Version, the
// platforms published for it, and the Source (display + trust metadata) it was
// discovered through.
type Plugin struct {
	install.Footprint
	Required  bool
	Version   string
	Platforms []string
	Source    SourceInfo
}

// Artifact is the resolved per-platform download: a blob-by-digest URL the
// reconciler fetches, plus the digest (as a bare sha256 hex) and size.
type Artifact struct {
	DownloadURL string
	Sha256      string
	SizeBytes   int64
}

// VersionStatus pairs a version tag with whether it has been promoted to the
// trusted namespace (Validated=true) or exists only in staging (Validated=false).
type VersionStatus struct {
	Version   string `json:"version"`
	Validated bool   `json:"validated"`
}

// repoAbsent reports whether err means "repository not available here", as
// opposed to a transport failure. A non-existent repo is a normal state (a
// plugin never promoted, or a staging repo emptied by the janitor's last
// delete), so callers treat it as an empty result and fall back to the staging
// namespace rather than erroring.
//
// GHCR status quirks this must absorb:
//   - 404: a registry that exposes a proper /v2 manifest 404 for a missing tag.
//   - 403 "denied": GHCR's token endpoint for a repo that does not exist.
//   - 401 "authentication required": GHCR's token endpoint for a PRIVATE repo
//     pulled anonymously. The desktop pulls anonymously, so a private (or
//     not-yet-public) plugin must degrade to "absent" rather than 503 the whole
//     catalog. The catalog reads anonymously, so 401 here is always treated as
//     absent (the authenticated StagingClient's tag listings never route through
//     this helper for that distinction).
func repoAbsent(err error) bool {
	if errors.Is(err, errdef.ErrNotFound) {
		return true
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized:
			return true
		}
	}
	return false
}

// fetchFootprint reads + decodes the plugin's footprint config blob from repo.
func fetchFootprint(ctx context.Context, repo *remote.Repository, cfg ocispec.Descriptor) (install.Footprint, error) {
	rc, err := repo.Fetch(ctx, cfg)
	if err != nil {
		return install.Footprint{}, fmt.Errorf("fetch config blob: %w", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return install.Footprint{}, err
	}
	var fp install.Footprint
	if err := json.Unmarshal(b, &fp); err != nil {
		return install.Footprint{}, fmt.Errorf("decode footprint config blob: %w", err)
	}
	return fp, nil
}

func fetchManifest(ctx context.Context, repo *remote.Repository, ref string) (ocispec.Manifest, error) {
	_, rc, err := repo.FetchReference(ctx, ref)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("fetch manifest %s: %w", ref, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ocispec.Manifest{}, err
	}
	var man ocispec.Manifest
	if err := json.Unmarshal(b, &man); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return man, nil
}
