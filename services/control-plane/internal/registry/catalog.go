package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// DefaultRequired is the control-plane policy set of plugins every workspace
// must have. Code-side on purpose (see package doc): whether a plugin is
// mandatory is control-plane policy, not something a plugin may self-declare.
var DefaultRequired = []string{"core-app", "core-datasource"}

// Registry is one OCI registry's coordinates, parsed from a per-plugin manifest.
type Registry struct {
	Host             string
	Namespace        string
	StagingNamespace string
	PublicURL        string
	PlainHTTP        bool
}

func (r *Registry) publicBase() string {
	if r.PublicURL != "" {
		return strings.TrimRight(r.PublicURL, "/")
	}
	return "https://" + r.Host
}

// repo builds an oras repository for (id) under ns in this registry.
func (r *Registry) repo(ns, id string, cl *auth.Client) (*remote.Repository, error) {
	repo, err := remote.NewRepository(r.Host + "/" + ns + "/" + id)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = r.PlainHTTP
	if cl != nil {
		repo.Client = cl
	}
	return repo, nil
}

// PluginRef is one plugin's resolution inputs: its manifest URL (identity),
// display + trust metadata, the registry hosting it, and its version sets
// (semver-desc).
type PluginRef struct {
	ManifestURL string
	PluginID    string
	Publisher   string
	Verified    bool
	Reg         *Registry
	Validated   []string
	Preview     []string
}

func (r *PluginRef) sourceInfo() SourceInfo {
	return SourceInfo{URL: r.ManifestURL, Publisher: r.Publisher, Verified: r.Verified}
}

// SourceInfo is the display + trust metadata surfaced on every catalog entry.
type SourceInfo struct {
	URL       string `json:"url"`
	Publisher string `json:"publisher"`
	Verified  bool   `json:"verified"`
}

// PluginProvider yields every plugin to show (official list ∪ user-added),
// fetched + parsed. Injected so the catalog stays decoupled from DB + manifest
// fetch and is fakeable in tests.
type PluginProvider interface {
	Plugins(ctx context.Context) ([]*PluginRef, error)
}

// Client is the federated catalog reader. No registry coords of its own — every
// coordinate comes from a PluginRef's Registry.
type Client struct {
	provider PluginProvider
	required map[string]bool
	anonAuth *auth.Client // nil = anonymous (reads never need creds)
}

// NewCatalog builds the catalog client over a provider. required is the
// control-plane policy set of mandatory plugin ids (official only).
func NewCatalog(p PluginProvider, required []string) *Client {
	req := make(map[string]bool, len(required))
	for _, r := range required {
		req[r] = true
	}
	return &Client{provider: p, required: req}
}

// IsRequired reports the control-plane policy for a plugin id without touching
// the registry (so uninstall's required-gate works even if the plugin was
// un-published).
func (c *Client) IsRequired(id string) bool { return c.required[id] }

// RequiredIDs returns the control-plane policy set of required plugin ids,
// sorted, without touching the registry. Onboarding installs exactly these
// (resolving each with the trusted->staging fallback) instead of building the
// full marketplace catalog.
func (c *Client) RequiredIDs() []string {
	ids := make([]string, 0, len(c.required))
	for id := range c.required {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// List returns the marketplace catalog: every plugin the provider yields that
// resolves to a footprint at its highest validated (else highest preview)
// version. Required plugins sort first, then by id.
func (c *Client) List(ctx context.Context) ([]Plugin, error) {
	refs, err := c.provider.Plugins(ctx)
	if err != nil {
		return nil, fmt.Errorf("provider plugins: %w", err)
	}
	var out []Plugin
	for _, ref := range refs {
		p, found, err := c.latest(ctx, ref)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Required != out[j].Required {
			return out[i].Required
		}
		return out[i].PluginID < out[j].PluginID
	})
	return out, nil
}

// latest reads footprint+platforms for a ref's highest validated version (or,
// if none validated, its highest preview version with Version="").
func (c *Client) latest(ctx context.Context, ref *PluginRef) (Plugin, bool, error) {
	version, ns, preview := pick(ref)
	if version == "" {
		return Plugin{}, false, nil
	}
	repo, err := ref.Reg.repo(ns, ref.PluginID, c.anonAuth)
	if err != nil {
		return Plugin{}, false, err
	}
	p, found, err := c.read(ctx, repo, ref, version)
	if err != nil || !found {
		return Plugin{}, found, err
	}
	if preview {
		p.Version = "" // preview-only: no validated version to advertise
	}
	p.Source = ref.sourceInfo()
	return p, true, nil
}

// pick chooses the version + namespace to read a ref's catalog footprint from:
// the highest validated version (trusted namespace) if any, else the highest
// preview version (staging namespace). preview=true on the staging path.
func pick(ref *PluginRef) (version, namespace string, preview bool) {
	if len(ref.Validated) > 0 {
		return ref.Validated[0], ref.Reg.Namespace, false
	}
	if len(ref.Preview) > 0 {
		return ref.Preview[0], ref.Reg.StagingNamespace, true
	}
	return "", "", false
}

// read fetches the footprint+platforms for a version from repo, tolerating the
// manifest's bare-semver form vs. the registry's v-prefixed tags by trying both
// tag forms. found=false (no error) when the repo or neither tag form exists.
func (c *Client) read(ctx context.Context, repo *remote.Repository, ref *PluginRef, version string) (Plugin, bool, error) {
	for _, tag := range tagForms(version) {
		man, err := fetchManifest(ctx, repo, tag)
		if err != nil {
			if repoAbsent(err) {
				continue
			}
			return Plugin{}, false, err
		}
		fp, err := fetchFootprint(ctx, repo, man.Config)
		if err != nil {
			return Plugin{}, false, err
		}
		platforms := make([]string, 0, len(man.Layers))
		for _, l := range man.Layers {
			if pl := l.Annotations[platformAnnotation]; pl != "" {
				platforms = append(platforms, pl)
			}
		}
		return Plugin{Footprint: fp, Required: c.required[ref.PluginID], Version: version, Platforms: platforms}, true, nil
	}
	return Plugin{}, false, nil
}

// tagForms returns the tag candidates for a version: a v-prefixed version is
// taken verbatim; a bare semver is tried verbatim then v-prefixed.
func tagForms(v string) []string {
	if strings.HasPrefix(v, "v") {
		return []string{v}
	}
	return []string{v, "v" + v}
}

// findRef returns the provider's PluginRef for an id. ok=false when no source
// yields that id.
func (c *Client) findRef(ctx context.Context, id string) (*PluginRef, bool, error) {
	refs, err := c.provider.Plugins(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, ref := range refs {
		if ref.PluginID == id {
			return ref, true, nil
		}
	}
	return nil, false, nil
}

// VersionsWithStatus returns every known version of a plugin, newest first. A
// version is Validated when it is in the ref's validated set; otherwise it is
// preview. The list is the union of the ref's validated and preview sets,
// compared on normalized semver so a version in both collapses to one entry
// (reported in its validated form). An unknown id yields an empty list.
func (c *Client) VersionsWithStatus(ctx context.Context, id string) ([]VersionStatus, error) {
	ref, ok, err := c.findRef(ctx, id)
	if err != nil || !ok {
		return []VersionStatus{}, err
	}
	validatedSet := map[string]bool{}
	for _, v := range ref.Validated {
		if n := normSemver(v); n != "" {
			validatedSet[n] = true
		}
	}
	repr := map[string]string{}
	for _, v := range append(append([]string{}, ref.Preview...), ref.Validated...) {
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

// ResolveArtifact returns the per-platform tarball blob for (id, version),
// trusted namespace first then staging. found=false when the plugin publishes
// no layer for that platform in either namespace.
func (c *Client) ResolveArtifact(ctx context.Context, id, version, platform string) (*Artifact, bool, error) {
	ref, ok, err := c.findRef(ctx, id)
	if err != nil || !ok {
		return nil, false, err
	}
	nss := []string{ref.Reg.Namespace}
	if ref.Reg.StagingNamespace != "" {
		nss = append(nss, ref.Reg.StagingNamespace)
	}
	for _, ns := range nss {
		repo, err := ref.Reg.repo(ns, id, c.anonAuth)
		if err != nil {
			return nil, false, err
		}
		for _, tag := range tagForms(version) {
			man, err := fetchManifest(ctx, repo, tag)
			if err != nil {
				if repoAbsent(err) {
					continue
				}
				return nil, false, err
			}
			for _, l := range man.Layers {
				if l.Annotations[platformAnnotation] != platform {
					continue
				}
				return &Artifact{
					DownloadURL: fmt.Sprintf("%s/v2/%s/%s/blobs/%s", ref.Reg.publicBase(), ns, id, l.Digest.String()),
					Sha256:      l.Digest.Encoded(),
					SizeBytes:   l.Size,
				}, true, nil
			}
		}
	}
	return nil, false, nil
}

// Get returns one plugin at its latest version (highest validated, else highest
// preview), reading from the appropriate namespace. found=false if the id is
// unknown across all sources. Used by install/onboarding/instance resolution.
func (c *Client) Get(ctx context.Context, id string) (Plugin, bool, error) {
	ref, ok, err := c.findRef(ctx, id)
	if err != nil || !ok {
		return Plugin{}, false, err
	}
	version, ns, _ := pick(ref)
	if version == "" {
		return Plugin{}, false, nil
	}
	repo, err := ref.Reg.repo(ns, id, c.anonAuth)
	if err != nil {
		return Plugin{}, false, err
	}
	p, found, err := c.read(ctx, repo, ref, version)
	if err != nil || !found {
		return Plugin{}, found, err
	}
	p.Source = ref.sourceInfo()
	return p, true, nil
}

// GetVersion returns footprint+platforms for an exact tag, trusted namespace
// first then staging. found=false when the tag exists in neither.
func (c *Client) GetVersion(ctx context.Context, id, tag string) (Plugin, bool, error) {
	ref, ok, err := c.findRef(ctx, id)
	if err != nil || !ok {
		return Plugin{}, false, err
	}
	nss := []string{ref.Reg.Namespace}
	if ref.Reg.StagingNamespace != "" {
		nss = append(nss, ref.Reg.StagingNamespace)
	}
	for _, ns := range nss {
		repo, err := ref.Reg.repo(ns, id, c.anonAuth)
		if err != nil {
			return Plugin{}, false, err
		}
		p, found, err := c.read(ctx, repo, ref, tag)
		if err != nil {
			return Plugin{}, false, err
		}
		if found {
			p.Source = ref.sourceInfo()
			return p, true, nil
		}
	}
	return Plugin{}, false, nil
}
