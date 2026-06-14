// Package registry reads the v8 plugin catalog from an OCI registry (GHCR).
// Each plugin is published as an OCI artifact whose config blob is the
// plugin's footprint config blob (its install Footprint, derived from plugin.json) and
// whose layers are per-platform tarballs, each annotated with its os-arch.
// This replaces the old hardcoded install.DefaultManifests map: publishing
// a plugin is now an `oras push`, and control-plane reads everything —
// footprint, version, per-platform artifact digest — from the registry at
// runtime.
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
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/portfolio-management/control-plane/internal/install"
)

// platformAnnotation must match the annotation plugindist stamps on each
// per-platform tarball layer.
const platformAnnotation = "io.opencapital.platform"

// ManifestSource supplies the plugin/version set from the public plugins
// manifest. It is the catalog's source of truth for which plugin ids exist and
// which of their versions are validated vs. preview — replacing the GitHub
// Packages REST enumeration of the trusted namespace AND the oras staging-tag
// listing the catalog used for preview versions. Injected (rather than imported
// concretely) so the registry stays decoupled from the manifest package and is
// trivially fakeable in tests.
type ManifestSource interface {
	// PluginIDs returns the manifest's validated-section plugin keys (including
	// ones with no validated version yet).
	PluginIDs(ctx context.Context) ([]string, error)
	// ValidatedVersions returns one plugin's validated versions, semver-desc.
	ValidatedVersions(ctx context.Context, id string) ([]string, error)
	// PreviewPluginIDs returns the manifest's preview-section plugin keys.
	PreviewPluginIDs(ctx context.Context) ([]string, error)
	// PreviewVersions returns one plugin's preview versions, semver-desc.
	PreviewVersions(ctx context.Context, id string) ([]string, error)
}

// DefaultRequired is the control-plane policy set of plugins every workspace
// must have. Code-side on purpose (see package doc).
var DefaultRequired = []string{"core-app", "core-datasource"}

// Plugin is a catalog entry: the plugin's self-described footprint plus
// control-plane-applied policy (Required) and the resolved latest Version +
// the platforms published for it.
type Plugin struct {
	install.Footprint
	Required  bool
	Version   string
	Platforms []string
}

// Artifact is the resolved per-platform download: a blob-by-digest URL the
// reconciler fetches, plus the digest (as a bare sha256 hex) and size.
type Artifact struct {
	DownloadURL string
	Sha256      string
	SizeBytes   int64
}

// Client reads plugins from the OCI registry. Stateless beyond config; each
// call hits the registry so a republish is visible immediately (always-
// latest; no cache in v0).
type Client struct {
	host             string // registry host without scheme
	plainHTTP        bool
	publicURL        string // host-reachable base for blob URLs
	namespace        string // repository prefix, e.g. "plugins"
	stagingNamespace string // staging repository prefix, e.g. "plugins-staging"
	required         map[string]bool
	basicAuth        *auth.Client   // REGISTRY_USERNAME/PASSWORD for GHCR and classic registries
	deleter          *GHCRDeleter   // GitHub Packages REST deleter for GHCR
	enum             RepoEnumerator // staging enumeration (ListStagingPluginIDs); replaces /v2/_catalog on GHCR
	manifest         ManifestSource // catalog source of truth for List + VersionsWithStatus (validated + preview)
}

// WithEnumerator sets the RepoEnumerator used by ListStagingPluginIDs (the
// staging janitor's enumeration). Required when targeting GHCR (whose
// /v2/_catalog is a global list, not per-owner).
func (c *Client) WithEnumerator(e RepoEnumerator) *Client { c.enum = e; return c }

// WithManifest sets the ManifestSource the marketplace catalog reads from. When
// set, List discovers plugin ids and both List and VersionsWithStatus derive
// the validated AND preview version sets from the public manifest file rather
// than enumerating the registry. When nil, both fall back to the
// trusted-namespace OCI enumeration.
func (c *Client) WithManifest(m ManifestSource) *Client { c.manifest = m; return c }

// WithGHCRDelete wires the GitHub Packages REST deleter used by DeleteStagingTag.
// Required when targeting GHCR (which does not support OCI manifest-DELETE).
func (c *Client) WithGHCRDelete(token string) *Client { c.deleter = NewGHCRDeleter(token); return c }

// repoClient returns the oras auth client for a concrete repo: basic-auth if
// configured, else nil (anonymous).
func (c *Client) repoClient() *auth.Client {
	return c.basicAuth
}

// New builds a Client. internalURL is how control-plane reaches the registry
// (e.g. https://ghcr.io). publicURL is the host-reachable base stamped into
// reconciler blob URLs. stagingNamespace is the repository prefix for
// unverified/candidate builds (e.g. "plugins-staging").
// username/password provide static basic-auth credentials (GHCR: owner + PAT).
func New(internalURL, publicURL, namespace, stagingNamespace string, required []string, username, password string) *Client {
	host := internalURL
	plainHTTP := false
	if rest, ok := strings.CutPrefix(host, "http://"); ok {
		host, plainHTTP = rest, true
	} else if rest, ok := strings.CutPrefix(host, "https://"); ok {
		host = rest
	}
	host = strings.TrimRight(host, "/")

	req := make(map[string]bool, len(required))
	for _, r := range required {
		req[r] = true
	}

	var client *auth.Client
	if username != "" {
		client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: auth.StaticCredential(host, auth.Credential{Username: username, Password: password}),
		}
	}

	return &Client{
		host:             host,
		plainHTTP:        plainHTTP,
		publicURL:        strings.TrimRight(publicURL, "/"),
		namespace:        strings.Trim(namespace, "/"),
		stagingNamespace: strings.Trim(stagingNamespace, "/"),
		required:         req,
		basicAuth:        client,
	}
}

// IsRequired reports the control-plane policy for a plugin id without
// touching the registry (so uninstall's required-gate works even if the
// plugin was un-published).
func (c *Client) IsRequired(id string) bool { return c.required[id] }

func (c *Client) repo(id string) (*remote.Repository, error) {
	name := c.namespace + "/" + id
	repo, err := remote.NewRepository(c.host + "/" + name)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = c.plainHTTP
	if cl := c.repoClient(); cl != nil {
		repo.Client = cl
	}
	return repo, nil
}

func (c *Client) stagingRepo(id string) (*remote.Repository, error) {
	name := c.stagingNamespace + "/" + id
	repo, err := remote.NewRepository(c.host + "/" + name)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = c.plainHTTP
	if cl := c.repoClient(); cl != nil {
		repo.Client = cl
	}
	return repo, nil
}

// CanPruneStaging reports whether the janitor has a delete capability wired. When
// false, the janitor still computes + logs its prune decisions but performs no deletes.
func (c *Client) CanPruneStaging() bool {
	return c.basicAuth != nil || c.deleter != nil
}

// StagingTagSigned reports whether <id>:<tag> in the staging namespace has a
// cosign signature referrer (the sigstore-bundle artifactType). This is the
// "Signed" input to the janitor predicate — a cheap referrers lookup, NOT a full
// cryptographic verification (the promotion gate does that). A staged tag with a
// signature referrer is treated as signed for retention purposes.
func (c *Client) StagingTagSigned(ctx context.Context, id, tag string) (bool, error) {
	repo, err := c.stagingRepo(id)
	if err != nil {
		return false, err
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return false, fmt.Errorf("resolve %s:%s: %w", id, tag, err)
	}
	_, found, err := findSignatureReferrer(ctx, repo, desc)
	if err != nil {
		return false, fmt.Errorf("find signature referrer %s:%s: %w", id, tag, err)
	}
	return found, nil
}

// DeleteStagingTag prunes <id>:<tag> from the STAGING namespace via the GitHub
// Packages REST API (resolves the package version carrying the tag, then deletes
// it). Returns an error if no GHCR deleter is wired.
func (c *Client) DeleteStagingTag(ctx context.Context, id, tag string) error {
	if c.deleter == nil {
		return fmt.Errorf("delete %s:%s: no GHCR deleter configured", id, tag)
	}
	pkg := c.stagingNamespace + "/" + id
	vid, ok, err := c.deleter.ResolveVersionID(ctx, pkg, tag)
	if err != nil {
		return fmt.Errorf("resolve %s:%s for delete: %w", id, tag, err)
	}
	if !ok {
		return nil // already gone — nothing to prune
	}
	if err := c.deleter.DeletePackageVersion(ctx, pkg, vid); err != nil {
		return fmt.Errorf("delete %s:%s: %w", id, tag, err)
	}
	return nil
}

// ListVersions returns the promoted (trusted-namespace) versions of a plugin,
// greatest semver first. A plugin that has NEVER been promoted has no trusted
// repository at all; the registry surfaces that as a 404 NameUnknown, which is
// not an error here — it means the promoted set is empty, so this returns nil
// rather than failing. (The staging janitor reads this set to decide "is this
// staged tag already promoted?", for which "no trusted repo" correctly means
// "no".)
func (c *Client) ListVersions(ctx context.Context, id string) ([]string, error) {
	repo, err := c.repo(id)
	if err != nil {
		return nil, err
	}
	var tags []string
	if err := repo.Tags(ctx, "", func(t []string) error { tags = append(tags, t...); return nil }); err != nil {
		if isRepoNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list tags: %w", err)
	}
	return sortSemverDesc(tags), nil
}

// isRepoNotFound reports whether err is the registry's "repository does not
// exist" response (HTTP 404 / NAME_UNKNOWN), as opposed to a transport or auth
// failure. A non-existent repo is a normal state (a plugin never promoted, or a
// staging repo emptied by the janitor's last delete), so callers treat it as an
// empty result rather than an error.
func isRepoNotFound(err error) bool {
	if errors.Is(err, errdef.ErrNotFound) {
		return true
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == http.StatusNotFound
	}
	return false
}

// ListStagingVersions returns the staged (staging-namespace) versions of a
// plugin, greatest semver first. Mirrors ListVersions but reads the staging
// repo so the promotion sweep can enumerate candidate tags.
func (c *Client) ListStagingVersions(ctx context.Context, id string) ([]string, error) {
	repo, err := c.stagingRepo(id)
	if err != nil {
		return nil, err
	}
	var tags []string
	if err := repo.Tags(ctx, "", func(t []string) error { tags = append(tags, t...); return nil }); err != nil {
		if isRepoNotFound(err) {
			return nil, nil // no staging repo = no staged versions, not an error
		}
		return nil, fmt.Errorf("list staging tags: %w", err)
	}
	return sortSemverDesc(tags), nil
}

// ListStagingPluginIDs returns the plugin ids present in the staging namespace.
// Used by the promotion sweep to discover candidates published since boot
// (a freshly-staged plugin may have no trusted repo yet).
func (c *Client) ListStagingPluginIDs(ctx context.Context) ([]string, error) {
	if c.enum == nil {
		return nil, fmt.Errorf("no repo enumerator configured")
	}
	ids, err := c.enum.ReposWithPrefix(ctx, c.stagingNamespace+"/")
	if err != nil {
		return nil, fmt.Errorf("enumerate staging repos: %w", err)
	}
	return ids, nil
}

// List returns the marketplace catalog. With a ManifestSource wired (the
// production path), the catalog spans every plugin that has EITHER a validated
// version OR a preview version in the public manifest:
//
//   - validated plugin: the entry is built from the footprint of its LATEST
//     validated version, and Version is that validated version (which the
//     handler surfaces as latest_validated_version).
//   - preview-only plugin (in the manifest's preview section with no validated
//     version yet): the entry is built from the footprint of its HIGHEST preview
//     version, read from the STAGING namespace at that exact version, with
//     Version="" so latest_validated_version is omitted — the card still renders
//     and install resolves via the trusted->staging fallback. A plugin with a
//     validated version wins on the validated path.
//
// BOTH the plugin ids and the version sets come entirely from the public
// manifest file — NO GitHub Packages REST or oras tag enumeration. The staging
// namespace is read only to fetch the footprint blob of the file-specified
// preview version.
//
// Without a ManifestSource it falls back to enumerating the trusted namespace
// via the RepoEnumerator (the legacy behavior), so a deploy that forgot to set
// PLUGINS_MANIFEST_URL still serves a catalog instead of a blank one.
func (c *Client) List(ctx context.Context) ([]Plugin, error) {
	if c.manifest == nil {
		return c.listFromTrustedEnum(ctx)
	}
	validatedIDs, err := c.manifest.PluginIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("manifest plugin ids: %w", err)
	}
	previewIDs, err := c.manifest.PreviewPluginIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("manifest preview plugin ids: %w", err)
	}
	seen := make(map[string]bool, len(validatedIDs)+len(previewIDs))
	ids := make([]string, 0, len(validatedIDs)+len(previewIDs))
	for _, id := range append(validatedIDs, previewIDs...) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	var out []Plugin
	for _, id := range ids {
		validated, err := c.manifest.ValidatedVersions(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("manifest validated versions %s: %w", id, err)
		}
		if len(validated) > 0 {
			// Validated path: latest validated version, Version set so the
			// handler advertises it as latest_validated_version. (unchanged)
			p, found, err := c.getValidatedVersion(ctx, id, validated[0]) // semver-desc
			if err != nil {
				return nil, err
			}
			if found {
				out = append(out, p)
			}
			continue
		}
		// Preview-only path: footprint from the highest preview version named in
		// the manifest, read from the staging namespace at that exact version.
		// Version="" — there is no validated version to advertise.
		preview, err := c.manifest.PreviewVersions(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("manifest preview versions %s: %w", id, err)
		}
		if len(preview) == 0 {
			continue
		}
		p, found, err := c.getStagingVersion(ctx, id, preview[0]) // semver-desc
		if err != nil {
			return nil, err
		}
		if found {
			p.Version = ""
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PluginID < out[j].PluginID })
	return out, nil
}

// getValidatedVersion reads the footprint+platforms of (id, version) from the
// trusted namespace, tolerating the manifest's bare-semver form vs. the
// registry's v-prefixed tags: it tries the version verbatim, then the
// v-prefixed variant. found=false only when neither tag exists.
func (c *Client) getValidatedVersion(ctx context.Context, id, version string) (Plugin, bool, error) {
	p, found, err := c.GetVersion(ctx, id, version)
	if err != nil {
		return Plugin{}, false, err
	}
	if found {
		return p, true, nil
	}
	if alt := "v" + version; alt != version && !strings.HasPrefix(version, "v") {
		return c.GetVersion(ctx, id, alt)
	}
	return Plugin{}, false, nil
}

// listFromTrustedEnum is the pre-manifest fallback: enumerate the trusted
// namespace and read each plugin at its latest promoted tag.
func (c *Client) listFromTrustedEnum(ctx context.Context) ([]Plugin, error) {
	if c.enum == nil {
		return nil, fmt.Errorf("no manifest source or repo enumerator configured")
	}
	ids, err := c.enum.ReposWithPrefix(ctx, c.namespace+"/")
	if err != nil {
		return nil, fmt.Errorf("enumerate repos: %w", err)
	}
	var out []Plugin
	for _, id := range ids {
		p, found, err := c.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PluginID < out[j].PluginID })
	return out, nil
}

// Get returns one plugin at its latest TRUSTED version. found=false when no
// version is published.
func (c *Client) Get(ctx context.Context, id string) (Plugin, bool, error) {
	repo, err := c.repo(id)
	if err != nil {
		return Plugin{}, false, err
	}
	return c.getLatestFrom(ctx, repo, id)
}

// getStagingVersion reads the footprint+platforms of (id, version) from the
// STAGING namespace, mirroring getValidatedVersion's bare-semver vs. v-prefixed
// tolerance: it tries the version verbatim, then the v-prefixed variant.
// found=false when the staging repo or neither tag form exists (a preview
// version named in the manifest whose staging artifact is missing). Used by List
// to surface preview-only plugins — the caller blanks Version so the catalog
// never advertises a staging tag as validated.
func (c *Client) getStagingVersion(ctx context.Context, id, version string) (Plugin, bool, error) {
	repo, err := c.stagingRepo(id)
	if err != nil {
		return Plugin{}, false, err
	}
	p, found, err := c.getVersionFrom(ctx, repo, id, version)
	if err != nil {
		return Plugin{}, false, err
	}
	if found {
		return p, true, nil
	}
	if alt := "v" + version; alt != version && !strings.HasPrefix(version, "v") {
		return c.getVersionFrom(ctx, repo, id, alt)
	}
	return Plugin{}, false, nil
}

// getLatestFrom resolves a plugin's latest published tag in the given repo and
// reads its footprint + platforms. A missing repo (no build in that namespace)
// is a normal empty state, surfaced as found=false rather than an error.
func (c *Client) getLatestFrom(ctx context.Context, repo *remote.Repository, id string) (Plugin, bool, error) {
	version, err := latestTag(ctx, repo)
	if err != nil {
		if isRepoNotFound(err) {
			return Plugin{}, false, nil
		}
		return Plugin{}, false, err
	}
	if version == "" {
		return Plugin{}, false, nil
	}
	man, err := fetchManifest(ctx, repo, version)
	if err != nil {
		return Plugin{}, false, err
	}
	fp, err := c.fetchFootprint(ctx, repo, man.Config)
	if err != nil {
		return Plugin{}, false, err
	}
	platforms := make([]string, 0, len(man.Layers))
	for _, l := range man.Layers {
		if p := l.Annotations[platformAnnotation]; p != "" {
			platforms = append(platforms, p)
		}
	}
	return Plugin{
		Footprint: fp,
		Required:  c.required[id],
		Version:   version,
		Platforms: platforms,
	}, true, nil
}

// GetVersion returns the footprint + platforms for a specific PROMOTED tag
// (trusted namespace), so an install can pin an exact version instead of
// always tracking latest. found=false when the tag is not present in the
// trusted repo (a never-promoted version, or a typo'd tag).
func (c *Client) GetVersion(ctx context.Context, id, tag string) (Plugin, bool, error) {
	repo, err := c.repo(id)
	if err != nil {
		return Plugin{}, false, err
	}
	return c.getVersionFrom(ctx, repo, id, tag)
}

// getVersionFrom reads the footprint + platforms for an exact tag from the given
// repo. found=false when the repo or tag is absent (a registry 404); auth
// (401/403) and transport errors propagate.
func (c *Client) getVersionFrom(ctx context.Context, repo *remote.Repository, id, tag string) (Plugin, bool, error) {
	man, err := fetchManifest(ctx, repo, tag)
	if err != nil {
		if isRepoNotFound(err) {
			return Plugin{}, false, nil
		}
		return Plugin{}, false, err
	}
	fp, err := c.fetchFootprint(ctx, repo, man.Config)
	if err != nil {
		return Plugin{}, false, err
	}
	platforms := make([]string, 0, len(man.Layers))
	for _, l := range man.Layers {
		if p := l.Annotations[platformAnnotation]; p != "" {
			platforms = append(platforms, p)
		}
	}
	return Plugin{
		Footprint: fp,
		Required:  c.required[id],
		Version:   tag,
		Platforms: platforms,
	}, true, nil
}

// VersionStatus pairs a version tag with whether it has been promoted to the
// trusted namespace (Validated=true) or exists only in staging (Validated=false).
type VersionStatus struct {
	Version   string `json:"version"`
	Validated bool   `json:"validated"`
}

// VersionsWithStatus returns every known version of a plugin, newest first. A
// version is Validated when it is in the public manifest's validated set;
// otherwise it is preview (Validated=false). The version LIST is the union of
// the manifest's validated and preview sets — both read from the file, NOT from
// an oras staging-tag listing.
//
// Versions are compared on normalized semver so a version present in both
// sections collapses to one entry; when that happens the validated (plugins)
// form is reported.
//
// Without a ManifestSource it falls back to the trusted-namespace enumeration
// (a version present in the trusted namespace is Validated).
func (c *Client) VersionsWithStatus(ctx context.Context, id string) ([]VersionStatus, error) {
	var validated, preview []string
	if c.manifest != nil {
		var err error
		validated, err = c.manifest.ValidatedVersions(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("manifest validated versions %s: %w", id, err)
		}
		preview, err = c.manifest.PreviewVersions(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("manifest preview versions %s: %w", id, err)
		}
	} else {
		var err error
		validated, err = c.ListVersions(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("list trusted versions: %w", err)
		}
	}

	validatedSet := make(map[string]bool, len(validated))
	for _, v := range validated {
		if n := normSemver(v); n != "" {
			validatedSet[n] = true
		}
	}

	// Union by normalized semver, preferring the validated form when a version
	// is in both sections.
	repr := make(map[string]string) // normalized -> original to report
	for _, v := range preview {
		if n := normSemver(v); n != "" {
			repr[n] = v
		}
	}
	for _, v := range validated {
		if n := normSemver(v); n != "" {
			repr[n] = v
		}
	}
	all := make([]string, 0, len(repr))
	for _, orig := range repr {
		all = append(all, orig)
	}
	all = sortSemverDesc(all)

	out := make([]VersionStatus, 0, len(all))
	for _, v := range all {
		out = append(out, VersionStatus{Version: v, Validated: validatedSet[normSemver(v)]})
	}
	return out, nil
}

// ResolveArtifact returns the per-platform tarball blob for (id, version).
// found=false when the plugin publishes no layer for that platform.
// Trusted namespace is consulted first; staging is the fallback so preview
// (not-yet-promoted) versions can be resolved.
func (c *Client) ResolveArtifact(ctx context.Context, id, version, platform string) (*Artifact, bool, error) {
	trusted, err := c.repo(id)
	if err != nil {
		return nil, false, err
	}
	if art, ok, err := c.resolveArtifactFrom(ctx, trusted, c.namespace, id, version, platform); err != nil {
		return nil, false, err
	} else if ok {
		return art, true, nil
	}
	staging, err := c.stagingRepo(id)
	if err != nil {
		return nil, false, err
	}
	return c.resolveArtifactFrom(ctx, staging, c.stagingNamespace, id, version, platform)
}

func (c *Client) resolveArtifactFrom(ctx context.Context, repo *remote.Repository, ns, id, version, platform string) (*Artifact, bool, error) {
	man, err := fetchManifest(ctx, repo, version)
	if err != nil {
		if isRepoNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	for _, l := range man.Layers {
		if l.Annotations[platformAnnotation] != platform {
			continue
		}
		return &Artifact{
			DownloadURL: fmt.Sprintf("%s/v2/%s/%s/blobs/%s", c.publicURL, ns, id, l.Digest.String()),
			Sha256:      l.Digest.Encoded(),
			SizeBytes:   l.Size,
		}, true, nil
	}
	return nil, false, nil
}

func (c *Client) fetchFootprint(ctx context.Context, repo *remote.Repository, cfg ocispec.Descriptor) (install.Footprint, error) {
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

// latestTag returns the highest published semver tag (v-prefixed), or ""
// when nothing is published or no tag is valid semver.
func latestTag(ctx context.Context, repo *remote.Repository) (string, error) {
	var tags []string
	err := repo.Tags(ctx, "", func(t []string) error {
		tags = append(tags, t...)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("list tags: %w", err)
	}
	return latestSemver(tags), nil
}
