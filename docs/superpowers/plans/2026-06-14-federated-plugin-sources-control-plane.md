# Federated Plugin Sources — Control-Plane Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the control-plane marketplace catalog federated — each plugin owns a self-describing per-plugin manifest (registry coords + its own version list); `plugins.json` becomes a curated list of pointers to those manifests; users add unknown plugins by manifest URL. Catalog entries are source-qualified (verified vs. third-party).

**Architecture:** Two file types — a per-plugin manifest (owned by each plugin's repo) and a curated `plugins.json` list of pointer URLs. A new `internal/sources` provider takes the official list URLs ∪ user-added URLs from the `plugin_sources` DB table, fetches+parses each per-plugin manifest, and yields `[]*registry.PluginRef`. The `registry` package splits into a **catalog** client (federated, manifest-driven, zero registry coords) and a **staging** client (the janitor's publish/prune path, keeps its own coords). The reconciler (`instance-bootstrap`) is untouched. This plan is the control-plane backend only; the desktop Sources UI is a separate follow-up plan.

**Tech Stack:** Go 1.x, `oras-go/v2` (OCI), `pgx`/`pgxpool` + `golang-migrate` (embedded SQL), `net/http` stdlib mux, `golang.org/x/mod/semver`.

**Reference spec:** `docs/superpowers/specs/2026-06-14-federated-plugin-sources-design.md`

---

## File Structure

**Create:**
- `services/control-plane/internal/registry/catalog.go` — `Registry`, `PluginRef`, `SourceInfo`, `PluginProvider`, and the catalog `Client` (federated read).
- `services/control-plane/internal/registry/staging.go` — `StagingClient`: the janitor's prune/promote-check methods, built from its own coords.
- `services/control-plane/internal/sources/sources.go` — `Provider` implementing `registry.PluginProvider`: official list ∪ DB `plugin_sources` → per-plugin manifests → `[]*PluginRef`.
- `services/control-plane/internal/sources/sources_test.go` — provider tests with fakes.
- `services/control-plane/internal/migrate/migrations/0026_plugin_sources.up.sql` / `.down.sql`.
- `services/control-plane/internal/httpapi/sources.go` + `sources_test.go` — source CRUD.
- `plugins/core-app.json`, `plugins/core-datasource.json`, `plugins/yfinance-app.json` — official per-plugin manifests (repo root `plugins/` dir).

**Delete (obsolete central promotion — replaced by manual `plugins.json` curation):**
- `.github/workflows/plugin-promote-check.yml`
- `.github/workflows/plugin-promote-reconcile.yml`
- `.github/actions/plugin-promote/action.yml` + `.github/actions/plugin-promote/reconcile.sh` (and the now-empty dir)
- `.github/plugins/signers.yaml`

**Modify:**
- `services/control-plane/internal/manifest/manifest.go` — two parsers: `PluginClient` (per-plugin manifest) + `ListClient` (marketplace list). + tests.
- `services/control-plane/internal/registry/registry.go` — gut the old single-registry `Client`; move catalog methods to `catalog.go`, janitor methods to `staging.go`. `Plugin` gains `Source SourceInfo`.
- `services/control-plane/internal/registry/registry_test.go`, `internal/httpapi/instance_test.go` — update `Client` construction to `NewCatalog(fakeProvider)`.
- `services/control-plane/internal/janitor/runner.go` — `New` takes `*registry.StagingClient`.
- `services/control-plane/internal/store/store.go` — `PluginSource` + CRUD.
- `services/control-plane/internal/config/config.go` — drop `RegistryPublicURL`; relabel the rest as janitor-only.
- `services/control-plane/cmd/control-plane/main.go` — build catalog provider + staging janitor client separately.
- `services/control-plane/internal/httpapi/httpapi.go` — `/v1/sources` routes.
- `services/control-plane/internal/httpapi/v1.go`, `plugins.go` — `source` on catalog DTOs.
- `plugins.json` (repo root) — convert to the list-of-URLs form.
- `opencapital-app/src-tauri/src/dataplane.rs` — drop all `REGISTRY_*` env from the local control-plane spawn.

---

## Phase A — Manifest formats

### Task 1: Per-plugin manifest parser + validation

**Files:**
- Modify: `services/control-plane/internal/manifest/manifest.go`
- Test: `services/control-plane/internal/manifest/manifest_test.go`

- [ ] **Step 1: Write the failing test** (replace `manifest_test.go`)

```go
package manifest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, body string, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const samplePlugin = `{
  "schemaVersion": 1,
  "pluginId": "acme-charting",
  "publisher": "Acme Corp",
  "registry": {
    "host": "ghcr.io",
    "namespace": "acme/oc-plugins",
    "stagingNamespace": "acme/oc-plugins-staging",
    "publicURL": "https://ghcr.io"
  },
  "versions": ["1.4.0", "1.3.0"],
  "preview": ["1.5.0-rc1"]
}`

func TestPluginClientParses(t *testing.T) {
	c := NewPluginClient(serve(t, samplePlugin, 200), nil, 0, nil)
	m, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.PluginID != "acme-charting" || m.Publisher != "Acme Corp" {
		t.Fatalf("bad meta: %+v", m)
	}
	if m.Registry.Namespace != "acme/oc-plugins" || m.Registry.StagingNamespace != "acme/oc-plugins-staging" {
		t.Fatalf("bad registry: %+v", m.Registry)
	}
	if len(m.Versions) != 2 || m.Versions[0] != "1.4.0" {
		t.Fatalf("bad versions: %v", m.Versions)
	}
}

func TestPluginClientValidation(t *testing.T) {
	cases := map[string]string{
		"no pluginId":      `{"schemaVersion":1,"registry":{"host":"h","namespace":"a/b"},"versions":[]}`,
		"no host":          `{"schemaVersion":1,"pluginId":"x","registry":{"namespace":"a/b"},"versions":[]}`,
		"no namespace":     `{"schemaVersion":1,"pluginId":"x","registry":{"host":"h"},"versions":[]}`,
		"preview no staging": `{"schemaVersion":1,"pluginId":"x","registry":{"host":"h","namespace":"a/b"},"versions":[],"preview":["1.0.0"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewPluginClient(serve(t, body, 200), nil, 0, nil)
			if _, err := c.Fetch(context.Background()); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/control-plane && go test ./internal/manifest/ -run TestPluginClient -v`
Expected: FAIL — `NewPluginClient` undefined.

- [ ] **Step 3: Rewrite `manifest.go`** — keep `New`'s caching skeleton fields (`url/httpc/ttl/now/log/mu/fetchedAt`), `DefaultTTL`. Replace the `doc` type + section getters with the per-plugin model + a generic cached client.

```go
type PluginManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	PluginID      string       `json:"pluginId"`
	Publisher     string       `json:"publisher"`
	Registry      RegistrySpec `json:"registry"`
	Versions      []string     `json:"versions"`
	Preview       []string     `json:"preview,omitempty"`
}

type RegistrySpec struct {
	Host             string `json:"host"`
	Namespace        string `json:"namespace"`
	StagingNamespace string `json:"stagingNamespace,omitempty"`
	PublicURL        string `json:"publicURL,omitempty"`
}

// PluginClient fetches + caches one per-plugin manifest URL.
type PluginClient struct {
	url   string
	httpc *http.Client
	ttl   time.Duration
	now   func() time.Time
	log   *slog.Logger

	mu        sync.Mutex
	cached    *PluginManifest
	fetchedAt time.Time
}

func NewPluginClient(url string, httpc *http.Client, ttl time.Duration, log *slog.Logger) *PluginClient {
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &PluginClient{url: url, httpc: httpc, ttl: ttl, now: time.Now, log: log}
}

func (c *PluginClient) Fetch(ctx context.Context) (*PluginManifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.cached, nil
	}
	body, err := getJSON(ctx, c.httpc, c.url)
	if err != nil {
		if c.cached != nil {
			c.log.Warn("plugin manifest refresh failed; serving cached", "err", err, "url", c.url)
			return c.cached, nil
		}
		return nil, fmt.Errorf("fetch plugin manifest %s: %w", c.url, err)
	}
	var m PluginManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode plugin manifest %s: %w", c.url, err)
	}
	if err := validatePlugin(&m); err != nil {
		return nil, fmt.Errorf("invalid plugin manifest %s: %w", c.url, err)
	}
	c.cached, c.fetchedAt = &m, c.now()
	return c.cached, nil
}

func validatePlugin(m *PluginManifest) error {
	if m.PluginID == "" {
		return errors.New("pluginId required")
	}
	if m.Registry.Host == "" {
		return errors.New("registry.host required")
	}
	if m.Registry.Namespace == "" {
		return errors.New("registry.namespace required")
	}
	if len(m.Preview) > 0 && m.Registry.StagingNamespace == "" {
		return errors.New("preview set but registry.stagingNamespace missing")
	}
	return nil
}

// getJSON GETs url and returns the body, erroring on non-200.
func getJSON(ctx context.Context, httpc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
```

Add imports: `errors`, `sync`, `io`. Delete the old `doc`/`load`/`fetch`/`PluginIDs`/`ValidatedVersions`/`PreviewPluginIDs`/`PreviewVersions`/`sortedKeys` and the old `New`/`Client`. Keep `normSemver`/`sortSemverDesc` only if `ListClient` (Task 2) or other code needs them; otherwise delete.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd services/control-plane && go test ./internal/manifest/ -run TestPluginClient -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/manifest/
git commit -m "feat(manifest): per-plugin self-describing manifest parser + validation"
```

---

### Task 2: Marketplace list parser

**Files:**
- Modify: `services/control-plane/internal/manifest/manifest.go`
- Test: `services/control-plane/internal/manifest/manifest_test.go`

- [ ] **Step 1: Write the failing test** (append)

```go
const sampleList = `{
  "schemaVersion": 1,
  "plugins": [
    "https://example.test/core-app.json",
    "https://example.test/core-datasource.json"
  ]
}`

func TestListClientParses(t *testing.T) {
	c := NewListClient(serve(t, sampleList, 200), nil, 0, nil)
	urls, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(urls) != 2 || urls[0] != "https://example.test/core-app.json" {
		t.Fatalf("bad urls: %v", urls)
	}
}

func TestListClientRejectsNonURL(t *testing.T) {
	c := NewListClient(serve(t, `{"plugins":["not a url"]}`, 200), nil, 0, nil)
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("expected validation error for non-URL entry")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/control-plane && go test ./internal/manifest/ -run TestListClient -v`
Expected: FAIL — `NewListClient` undefined.

- [ ] **Step 3: Add the `ListClient`** to `manifest.go`

```go
type listDoc struct {
	SchemaVersion int      `json:"schemaVersion"`
	Plugins       []string `json:"plugins"`
}

// ListClient fetches + caches the curated marketplace list (array of per-plugin
// manifest URLs).
type ListClient struct {
	url   string
	httpc *http.Client
	ttl   time.Duration
	now   func() time.Time
	log   *slog.Logger

	mu        sync.Mutex
	cached    []string
	fetchedAt time.Time
}

func NewListClient(url string, httpc *http.Client, ttl time.Duration, log *slog.Logger) *ListClient {
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &ListClient{url: url, httpc: httpc, ttl: ttl, now: time.Now, log: log}
}

func (c *ListClient) Fetch(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.cached, nil
	}
	body, err := getJSON(ctx, c.httpc, c.url)
	if err != nil {
		if c.cached != nil {
			c.log.Warn("marketplace list refresh failed; serving cached", "err", err, "url", c.url)
			return c.cached, nil
		}
		return nil, fmt.Errorf("fetch marketplace list %s: %w", c.url, err)
	}
	var d listDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("decode marketplace list: %w", err)
	}
	for _, u := range d.Plugins {
		if pu, perr := url.Parse(u); perr != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
			return nil, fmt.Errorf("marketplace list entry %q is not an http(s) URL", u)
		}
	}
	c.cached, c.fetchedAt = d.Plugins, c.now()
	return c.cached, nil
}
```

Add `net/url` to imports.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd services/control-plane && go test ./internal/manifest/ -v`
Expected: PASS (all manifest tests).

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/manifest/
git commit -m "feat(manifest): marketplace list (pointer array) parser"
```

---

## Phase B — Registry split (catalog vs. staging)

### Task 3: Catalog types

**Files:**
- Create: `services/control-plane/internal/registry/catalog.go`
- Test: `services/control-plane/internal/registry/catalog_test.go`
- Modify: `services/control-plane/internal/registry/registry.go` (add `Source` to `Plugin`)

- [ ] **Step 1: Write the failing test**

```go
package registry

import (
	"context"
	"testing"
)

type fakeProvider struct{ refs []*PluginRef }

func (f fakeProvider) Plugins(context.Context) ([]*PluginRef, error) { return f.refs, nil }

func TestRegistryPublicBase(t *testing.T) {
	if (&Registry{Host: "ghcr.io"}).publicBase() != "https://ghcr.io" {
		t.Fatal("default publicBase wrong")
	}
	if (&Registry{Host: "ghcr.io", PublicURL: "https://cdn.x"}).publicBase() != "https://cdn.x" {
		t.Fatal("explicit publicBase wrong")
	}
}

func TestSourceInfoFromRef(t *testing.T) {
	r := &PluginRef{ManifestURL: "u", Publisher: "Acme", Verified: false}
	if got := r.sourceInfo(); got.URL != "u" || got.Publisher != "Acme" || got.Verified {
		t.Fatalf("sourceInfo = %+v", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/control-plane && go test ./internal/registry/ -run 'TestRegistryPublicBase|TestSourceInfo' -v`
Expected: FAIL — undefined types.

- [ ] **Step 3: Create `catalog.go`** with the types + an empty `Client` shell (filled in Task 4)

```go
package registry

import (
	"context"
	"strings"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

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

func (c *Client) IsRequired(id string) bool { return c.required[id] }
```

In `registry.go`, add `Source SourceInfo` to the `Plugin` struct, and keep `RequiredIDs` (move it to `catalog.go` if it referenced removed fields — it only reads `c.required`, so it works on the new `Client`).

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd services/control-plane && go test ./internal/registry/ -run 'TestRegistryPublicBase|TestSourceInfo' -v`
Expected: PASS (after Task 4 the package fully compiles; this sub-test only needs the types).

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/registry/catalog.go services/control-plane/internal/registry/catalog_test.go services/control-plane/internal/registry/registry.go
git commit -m "feat(registry): catalog types (PluginRef/Registry/PluginProvider) + Plugin.Source"
```

---

### Task 4: Catalog `Client` resolution over the provider

**Files:**
- Modify: `services/control-plane/internal/registry/catalog.go`, `registry.go`
- Test: `services/control-plane/internal/registry/catalog_test.go`

- [ ] **Step 1: Add `List`, `findRef`, `VersionsWithStatus`, `ResolveArtifact` to `catalog.go`**

```go
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

func pick(ref *PluginRef) (version, namespace string, preview bool) {
	if len(ref.Validated) > 0 {
		return ref.Validated[0], ref.Reg.Namespace, false
	}
	if len(ref.Preview) > 0 {
		return ref.Preview[0], ref.Reg.StagingNamespace, true
	}
	return "", "", false
}

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

func tagForms(v string) []string {
	if strings.HasPrefix(v, "v") {
		return []string{v}
	}
	return []string{v, "v" + v}
}

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
```

Add imports to `catalog.go`: `fmt`, `sort`, `ocispec "github.com/opencontainers/image-spec/specs-go/v1"` is not needed here (only via helpers). Ensure `fetchManifest`, `fetchFootprint`, `repoAbsent`, `normSemver`, `sortSemverDesc`, `platformAnnotation`, `Plugin`, `Artifact`, `VersionStatus` remain in `registry.go`/`semver.go`.

- [ ] **Step 2: Convert `registry.go` survivors**

In `registry.go`: make `repoAbsent` a free function `func repoAbsent(err error) bool` (it had a `c.basicAuth == nil` branch — for the anonymous catalog that branch is always true, so the `http.StatusUnauthorized` case returns `true`). Make `fetchFootprint` a free function (it was a `Client` method). Delete the entire old `Client` struct, `New`, `repo`, `stagingRepo`, `CanPruneStaging`, `StagingTagSigned`, `DeleteStagingTag`, `ListVersions`, `ListStagingVersions`, `ListStagingPluginIDs`, `List`, `Get`, `getStagingVersion`, `getLatestFrom`, `GetVersion`, `getVersionFrom`, `ResolveArtifact`, `resolveArtifactFrom`, `getValidatedVersion`, `listFromTrustedEnum`, `fetchFootprint(method)`, `WithEnumerator`, `WithManifest`, `WithGHCRDelete`, `repoClient`, `IsRequired`, `RequiredIDs`, `DefaultRequired`, `ManifestSource`. Keep ONLY: `Plugin`, `Artifact`, `VersionStatus`, `platformAnnotation`, `fetchManifest`, `fetchFootprint`(free), `latestTag`/`latestSemver` if still used, `repoAbsent`(free), and re-add `DefaultRequired` + `RequiredIDs` to `catalog.go`. (Move `DefaultRequired` and `RequiredIDs` to `catalog.go`.)

Add to `catalog.go`:

```go
// DefaultRequired is the control-plane policy set every workspace must have.
var DefaultRequired = []string{"core-app", "core-datasource"}

func (c *Client) RequiredIDs() []string {
	ids := make([]string, 0, len(c.required))
	for id := range c.required {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
```

- [ ] **Step 3: Add a List test with a stub OCI registry**

```go
func TestListEmptyWhenRegistryAbsent(t *testing.T) {
	ref := &PluginRef{
		ManifestURL: "u", PluginID: "x", Publisher: "Acme",
		Reg: &Registry{Host: "127.0.0.1:0"}, Validated: []string{"1.0.0"},
	}
	c := NewCatalog(fakeProvider{refs: []*PluginRef{ref}}, nil)
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty (registry unreachable → absent), got %d", len(got))
	}
}
```

(`127.0.0.1:0` is unroutable → `repoAbsent` swallows it → empty, not error. Matches the existing `instance_test.go` stub convention.)

- [ ] **Step 4: Update `instance_test.go` + `registry_test.go` constructions**

Replace every `registry.New("http://127.0.0.1:0", "", "plugins", "plugins-staging", nil, "", "")` with `registry.NewCatalog(<fakeProvider or nil-provider>, nil)`. For handler auth-guard tests that never reach the registry, pass a provider returning an error or empty:

```go
reg := registry.NewCatalog(stubProvider{}, nil) // stubProvider.Plugins returns nil,nil
```

Define `stubProvider` in the test file. Delete assertions that depended on old single-namespace behavior.

- [ ] **Step 5: Run the registry tests**

Run: `cd services/control-plane && go test ./internal/registry/ -v`
Expected: PASS. Iterate on compile errors.

- [ ] **Step 6: Commit**

```bash
git add services/control-plane/internal/registry/
git commit -m "refactor(registry): federated catalog Client over PluginProvider"
```

---

### Task 5: Staging client (janitor) split

**Files:**
- Create: `services/control-plane/internal/registry/staging.go`
- Modify: `services/control-plane/internal/janitor/runner.go`
- Test: `services/control-plane/internal/registry/staging_test.go`

- [ ] **Step 1: Create `staging.go`** — move the janitor methods here, built from explicit coords

```go
package registry

import (
	"context"
	"fmt"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
	"strings"
)

// StagingClient is the janitor's view of OpenCapital's own registry: it lists +
// signs-checks + prunes the staging namespace and checks the trusted namespace
// for already-promoted tags. Built from REGISTRY_* config (publish path),
// decoupled from the federated catalog.
type StagingClient struct {
	host             string
	plainHTTP        bool
	namespace        string
	stagingNamespace string
	basicAuth        *auth.Client
	deleter          *GHCRDeleter
	enum             RepoEnumerator
}

func NewStaging(internalURL, namespace, stagingNamespace, username, password string) *StagingClient {
	host := internalURL
	plainHTTP := false
	if rest, ok := strings.CutPrefix(host, "http://"); ok {
		host, plainHTTP = rest, true
	} else if rest, ok := strings.CutPrefix(host, "https://"); ok {
		host = rest
	}
	host = strings.TrimRight(host, "/")
	s := &StagingClient{host: host, plainHTTP: plainHTTP,
		namespace: strings.Trim(namespace, "/"), stagingNamespace: strings.Trim(stagingNamespace, "/")}
	if username != "" {
		s.basicAuth = &auth.Client{
			Client: retry.DefaultClient, Cache: auth.NewCache(),
			Credential: auth.StaticCredential(host, auth.Credential{Username: username, Password: password}),
		}
	}
	return s
}

func (s *StagingClient) WithEnumerator(e RepoEnumerator) *StagingClient { s.enum = e; return s }
func (s *StagingClient) WithGHCRDelete(token string) *StagingClient      { s.deleter = NewGHCRDeleter(token); return s }
func (s *StagingClient) CanPruneStaging() bool                            { return s.basicAuth != nil || s.deleter != nil }

func (s *StagingClient) repo(ns, id string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(s.host + "/" + ns + "/" + id)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = s.plainHTTP
	if s.basicAuth != nil {
		repo.Client = s.basicAuth
	}
	return repo, nil
}
```

Then move (verbatim, adjusting receiver to `*StagingClient` and `c.namespace/c.stagingNamespace` to `s.*`): `ListVersions`, `ListStagingVersions`, `ListStagingPluginIDs`, `StagingTagSigned`, `DeleteStagingTag` from the old `registry.go`. Each uses `s.repo(...)`, `repoAbsent`, `findSignatureReferrer`, `sortSemverDesc`, `latestTag` as before. `DeleteStagingTag` uses `s.deleter` + `s.stagingNamespace`. `ListStagingPluginIDs` uses `s.enum.ReposWithPrefix(ctx, s.stagingNamespace+"/")`.

- [ ] **Step 2: Update the janitor** `runner.go`

Change `janitor.New(reg *registry.Client, ...)` to `janitor.New(reg *registry.StagingClient, ...)`. The method calls are identical (same names). If `runner.go` references `registry.Client` in a field type, change it to `*registry.StagingClient`.

- [ ] **Step 3: Write a construction test**

```go
func TestNewStagingCanPrune(t *testing.T) {
	if NewStaging("https://ghcr.io", "p", "p-staging", "", "").CanPruneStaging() {
		t.Fatal("no creds, no deleter → cannot prune")
	}
	if !NewStaging("https://ghcr.io", "p", "p-staging", "user", "pat").CanPruneStaging() {
		t.Fatal("basic auth → can prune")
	}
}
```

- [ ] **Step 4: Run tests**

Run: `cd services/control-plane && go test ./internal/registry/ ./internal/janitor/ -v`
Expected: PASS. Update janitor test construction to `registry.NewStaging(...)`.

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/registry/staging.go services/control-plane/internal/janitor/
git commit -m "refactor(registry): split janitor staging client from catalog"
```

---

## Phase C — Source store

### Task 6: `plugin_sources` migration

**Files:**
- Create: `services/control-plane/internal/migrate/migrations/0026_plugin_sources.up.sql` / `.down.sql`

- [ ] **Step 1: up**

```sql
-- User-added federated plugin sources. Each row is a per-plugin manifest URL the
-- user subscribed to. The OFFICIAL set is NOT stored here — it is the live
-- plugins.json list fetch. Registry coords + versions live in each manifest
-- (spec §3); only the URL + cached display publisher are stored.
CREATE TABLE IF NOT EXISTS plugin_sources (
    manifest_url TEXT PRIMARY KEY,
    publisher    TEXT NOT NULL DEFAULT '',
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    added_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

- [ ] **Step 2: down**

```sql
DROP TABLE IF EXISTS plugin_sources;
```

- [ ] **Step 3: Build**

Run: `cd services/control-plane && go build ./internal/migrate/`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add services/control-plane/internal/migrate/migrations/0026_plugin_sources.*.sql
git commit -m "feat(db): plugin_sources table (user-added manifest URLs)"
```

---

### Task 7: Store CRUD

**Files:**
- Modify: `services/control-plane/internal/store/store.go`

- [ ] **Step 1: Add the struct + methods**

```go
// PluginSource is one user-added per-plugin manifest URL.
type PluginSource struct {
	ManifestURL string
	Publisher   string
	Enabled     bool
	AddedAt     time.Time
}

func (s *Store) ListPluginSources(ctx context.Context) ([]PluginSource, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT manifest_url, publisher, enabled, added_at FROM plugin_sources ORDER BY added_at`)
	if err != nil {
		return nil, fmt.Errorf("list plugin_sources: %w", err)
	}
	defer rows.Close()
	var out []PluginSource
	for rows.Next() {
		var p PluginSource
		if err := rows.Scan(&p.ManifestURL, &p.Publisher, &p.Enabled, &p.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CreatePluginSource(ctx context.Context, url, publisher string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO plugin_sources (manifest_url, publisher) VALUES ($1, $2)`, url, publisher)
	if err != nil {
		return fmt.Errorf("create plugin_source: %w", err)
	}
	return nil
}

// DeletePluginSource removes a user-added source. Returns (deleted, error).
func (s *Store) DeletePluginSource(ctx context.Context, url string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM plugin_sources WHERE manifest_url = $1`, url)
	if err != nil {
		return false, fmt.Errorf("delete plugin_source: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) UpdateSourcePublisher(ctx context.Context, url, publisher string) error {
	_, err := s.pool.Exec(ctx, `UPDATE plugin_sources SET publisher = $2 WHERE manifest_url = $1`, url, publisher)
	return err
}
```

- [ ] **Step 2: Build**

Run: `cd services/control-plane && go build ./internal/store/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add services/control-plane/internal/store/store.go
git commit -m "feat(store): plugin_sources CRUD"
```

---

## Phase D — Provider

### Task 8: `internal/sources` provider

**Files:**
- Create: `services/control-plane/internal/sources/sources.go`, `sources_test.go`

- [ ] **Step 1: Write the failing test**

```go
package sources

import (
	"context"
	"testing"

	"github.com/portfolio-management/control-plane/internal/manifest"
	"github.com/portfolio-management/control-plane/internal/store"
)

type fakeStore struct{ rows []store.PluginSource }

func (f fakeStore) ListPluginSources(context.Context) ([]store.PluginSource, error) { return f.rows, nil }

type fakeList []string

func (f fakeList) Fetch(context.Context) ([]string, error) { return []string(f), nil }

type fakePlugins map[string]*manifest.PluginManifest

func (f fakePlugins) Fetch(_ context.Context, url string) (*manifest.PluginManifest, error) {
	return f[url], nil
}

func TestProviderUnionAndVerified(t *testing.T) {
	core := &manifest.PluginManifest{PluginID: "core-app", Publisher: "OpenCapital",
		Registry: manifest.RegistrySpec{Host: "ghcr.io", Namespace: "oc/plugins", StagingNamespace: "oc/plugins-staging"},
		Versions: []string{"0.1.2"}}
	acme := &manifest.PluginManifest{PluginID: "acme-charting", Publisher: "Acme",
		Registry: manifest.RegistrySpec{Host: "ghcr.io", Namespace: "acme/p"},
		Versions: []string{"1.4.0"}}
	p := New(
		fakeStore{rows: []store.PluginSource{{ManifestURL: "acme-url", Enabled: true}}},
		fakeList{"core-url"},
		fakePlugins{"core-url": core, "acme-url": acme},
	)
	refs, err := p.Plugins(context.Background())
	if err != nil {
		t.Fatalf("Plugins: %v", err)
	}
	byID := map[string]bool{}
	for _, r := range refs {
		byID[r.PluginID] = r.Verified
	}
	if !byID["core-app"] {
		t.Fatal("official (listed) plugin must be verified")
	}
	if byID["acme-charting"] {
		t.Fatal("user-added plugin must not be verified")
	}
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %d", len(refs))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd services/control-plane && go test ./internal/sources/ -v`
Expected: FAIL — package/`New` undefined.

- [ ] **Step 3: Create `sources.go`**

```go
// Package sources builds the registry.PluginProvider from the official
// marketplace list ∪ user-added DB rows: it fetches + parses each per-plugin
// manifest and tags the listed ones verified.
package sources

import (
	"context"
	"fmt"
	"sort"

	"github.com/portfolio-management/control-plane/internal/manifest"
	"github.com/portfolio-management/control-plane/internal/registry"
	"github.com/portfolio-management/control-plane/internal/store"
	"golang.org/x/mod/semver"
)

type SourceStore interface {
	ListPluginSources(ctx context.Context) ([]store.PluginSource, error)
}

// ListFetcher fetches the curated marketplace list (manifest.ListClient).
type ListFetcher interface {
	Fetch(ctx context.Context) ([]string, error)
}

// PluginFetcher fetches+parses one per-plugin manifest URL (cached per URL).
type PluginFetcher interface {
	Fetch(ctx context.Context, url string) (*manifest.PluginManifest, error)
}

type Provider struct {
	store   SourceStore
	list    ListFetcher
	plugins PluginFetcher
}

func New(st SourceStore, list ListFetcher, plugins PluginFetcher) *Provider {
	return &Provider{store: st, list: list, plugins: plugins}
}

func (p *Provider) Plugins(ctx context.Context) ([]*registry.PluginRef, error) {
	// Official URLs (verified). A list-fetch failure degrades to empty rather
	// than blanking user-added plugins.
	officialURLs, _ := p.list.Fetch(ctx)
	official := make(map[string]bool, len(officialURLs))
	order := append([]string{}, officialURLs...)
	for _, u := range officialURLs {
		official[u] = true
	}
	// User URLs (skip any already in the official list — dedup by URL).
	rows, err := p.store.ListPluginSources(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	for _, row := range rows {
		if row.Enabled && !official[row.ManifestURL] {
			order = append(order, row.ManifestURL)
		}
	}
	var out []*registry.PluginRef
	for _, url := range order {
		m, err := p.plugins.Fetch(ctx, url)
		if err != nil || m == nil {
			continue // one unreachable manifest must not blank the catalog
		}
		out = append(out, &registry.PluginRef{
			ManifestURL: url,
			PluginID:    m.PluginID,
			Publisher:   m.Publisher,
			Verified:    official[url],
			Reg: &registry.Registry{
				Host: m.Registry.Host, Namespace: m.Registry.Namespace,
				StagingNamespace: m.Registry.StagingNamespace, PublicURL: m.Registry.PublicURL,
			},
			Validated: sortDesc(m.Versions),
			Preview:   sortDesc(m.Preview),
		})
	}
	return out, nil
}

func sortDesc(vs []string) []string {
	norm := func(v string) string {
		if semver.IsValid(v) {
			return v
		}
		if p := "v" + v; semver.IsValid(p) {
			return p
		}
		return ""
	}
	out := append([]string{}, vs...)
	sort.Slice(out, func(i, j int) bool { return semver.Compare(norm(out[i]), norm(out[j])) > 0 })
	return out
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd services/control-plane && go test ./internal/sources/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/sources/
git commit -m "feat(sources): official-list ∪ user-URL PluginProvider"
```

---

## Phase E — Config + main + data

### Task 9: Drop `RegistryPublicURL`; relabel the rest janitor-only

**Files:**
- Modify: `services/control-plane/internal/config/config.go`

- [ ] **Step 1: Remove `RegistryPublicURL`**

Delete the `RegistryPublicURL` struct field and its `cfg.RegistryPublicURL = ...` line. Keep `RegistryInternalURL`, `RegistryNamespace`, `RegistryStagingNamespace`, `RegistryOwner`, `PluginsManifestURL`. Update the block doc comment:

```go
	// Janitor / staging publish path ONLY (not federated discovery): the GHCR
	// host, trusted + staging namespaces, and owner the staging janitor prunes.
	// The catalog reads registry coordinates from each per-plugin manifest, not
	// from these. REGISTRY_USERNAME/PASSWORD are read directly in main.go.
	RegistryInternalURL      string
	RegistryNamespace        string
	RegistryStagingNamespace string
	RegistryOwner            string

	// PluginsManifestURL is the curated marketplace LIST (array of per-plugin
	// manifest URLs) the catalog seeds the official set from. Loaded from
	// PLUGINS_MANIFEST_URL.
	PluginsManifestURL string
```

- [ ] **Step 2: Build the package**

Run: `cd services/control-plane && go build ./internal/config/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add services/control-plane/internal/config/config.go
git commit -m "refactor(config): drop RegistryPublicURL; mark REGISTRY_* janitor-only"
```

---

### Task 10: Rewire `main.go`

**Files:**
- Modify: `services/control-plane/cmd/control-plane/main.go`

- [ ] **Step 1: Replace the `reg := registry.New(...)` block (lines ~134-161)**

```go
	if cfg.PluginsManifestURL == "" {
		logger.Error("PLUGINS_MANIFEST_URL is required (curated marketplace list)")
		os.Exit(1)
	}
	// Federated catalog: official list ∪ user-added DB rows → per-plugin
	// manifests. One cached PluginClient per URL behind a small adapter.
	listClient := manifest.NewListClient(cfg.PluginsManifestURL, nil, manifest.DefaultTTL, logger)
	pf := &pluginFetcher{ttl: manifest.DefaultTTL, log: logger, clients: map[string]*manifest.PluginClient{}}
	provider := sources.New(st, listClient, pf)
	reg := registry.NewCatalog(provider, registry.DefaultRequired)
	logger.Info("federated plugin catalog ready", "marketplace_list", cfg.PluginsManifestURL)

	// Staging janitor client (publish/prune path; its own coords + creds).
	staging := registry.NewStaging(cfg.RegistryInternalURL, cfg.RegistryNamespace,
		cfg.RegistryStagingNamespace, os.Getenv("REGISTRY_USERNAME"), os.Getenv("REGISTRY_PASSWORD"))
	if ghToken := os.Getenv("REGISTRY_PASSWORD"); ghToken != "" && cfg.RegistryOwner != "" {
		staging = staging.WithEnumerator(registry.NewGHCREnumerator(ghToken)).WithGHCRDelete(ghToken)
		logger.Info("staging janitor REST enumeration", "owner", cfg.RegistryOwner)
	}
```

- [ ] **Step 2: Change the janitor construction (line ~187)** to use `staging`

```go
	jan := janitor.New(staging, logger)
	logger.Info("staging janitor ready", "delete_enabled", staging.CanPruneStaging())
	go jan.Run(ctx)
```

- [ ] **Step 3: Add the `pluginFetcher` adapter at the bottom of `main.go`**

```go
// pluginFetcher adapts per-URL manifest.PluginClient instances to
// sources.PluginFetcher, reusing one cached client per URL.
type pluginFetcher struct {
	ttl     time.Duration
	log     *slog.Logger
	mu      sync.Mutex
	clients map[string]*manifest.PluginClient
}

func (f *pluginFetcher) Fetch(ctx context.Context, url string) (*manifest.PluginManifest, error) {
	f.mu.Lock()
	c, ok := f.clients[url]
	if !ok {
		c = manifest.NewPluginClient(url, nil, f.ttl, f.log)
		f.clients[url] = c
	}
	f.mu.Unlock()
	return c.Fetch(ctx)
}
```

Add imports `sync`, `sources`; ensure `manifest`, `registry` imported. `httpapi.New(...)` still receives `reg` (now the catalog `*registry.Client`).

- [ ] **Step 4: Build + test the whole service**

Run: `cd services/control-plane && go build ./... && go test ./...`
Expected: PASS. Fix any leftover references to removed registry symbols.

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/cmd/control-plane/main.go
git commit -m "feat(control-plane): wire federated catalog + standalone staging janitor"
```

---

### Task 11: `plugins.json` list form + official per-plugin manifests

**Files:**
- Modify: `plugins.json`
- Create: `plugins/core-app.json`, `plugins/core-datasource.json`, `plugins/yfinance-app.json`

- [ ] **Step 1: Rewrite `plugins.json`** to the pointer list

```json
{
  "schemaVersion": 1,
  "plugins": [
    "https://raw.githubusercontent.com/opencapital-dev/opencapital/main/plugins/core-app.json",
    "https://raw.githubusercontent.com/opencapital-dev/opencapital/main/plugins/core-datasource.json",
    "https://raw.githubusercontent.com/opencapital-dev/opencapital/main/plugins/yfinance-app.json"
  ]
}
```

- [ ] **Step 2: Create the three per-plugin manifests** (versions from the old `plugins.json`)

`plugins/core-app.json`:
```json
{
  "schemaVersion": 1,
  "pluginId": "core-app",
  "publisher": "OpenCapital",
  "registry": {
    "host": "ghcr.io",
    "namespace": "opencapital-dev/plugins",
    "stagingNamespace": "opencapital-dev/plugins-staging",
    "publicURL": "https://ghcr.io"
  },
  "versions": ["0.1.2"],
  "preview": []
}
```

`plugins/core-datasource.json` — identical but `"pluginId": "core-datasource"`, `"versions": ["0.1.7"]`.
`plugins/yfinance-app.json` — identical but `"pluginId": "yfinance-app"`, `"versions": ["0.1.2"]`.

- [ ] **Step 3: Confirm both formats parse** (smoke via the Task 1/2 tests already green)

Run: `cd services/control-plane && go test ./internal/manifest/ -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add plugins.json plugins/
git commit -m "chore(catalog): plugins.json -> pointer list; add per-plugin manifests"
```

---

### Task 12: Drop `REGISTRY_*` env from the desktop control-plane spawn

**Files:**
- Modify: `opencapital-app/src-tauri/src/dataplane.rs:198-210`

- [ ] **Step 1: Remove the four registry-coord env entries**

Delete `REGISTRY_INTERNAL_URL`, `REGISTRY_PUBLIC_URL`, `REGISTRY_NAMESPACE`, `REGISTRY_STAGING_NAMESPACE` from `spawn_control_plane`'s env array. Keep `PLUGINS_MANIFEST_URL`. Comment:

```rust
        // Catalog coords come from each per-plugin manifest; the local
        // control-plane's staging janitor no-ops without creds. Only the
        // curated marketplace list URL is needed.
        ("PLUGINS_MANIFEST_URL", "https://raw.githubusercontent.com/opencapital-dev/opencapital/main/plugins.json"),
```

- [ ] **Step 2: Build the shell**

Run: `cd opencapital-app/src-tauri && cargo build`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add opencapital-app/src-tauri/src/dataplane.rs
git commit -m "chore(desktop): drop REGISTRY_* from local control-plane env"
```

---

## Phase F — Source-qualified DTOs + CRUD API

### Task 13: Add `source` to catalog DTOs

**Files:**
- Modify: `services/control-plane/internal/httpapi/v1.go`, `plugins.go`
- Test: `services/control-plane/internal/httpapi/v1_test.go`

- [ ] **Step 1: Extend the DTOs** — add `Source registry.SourceInfo \`json:"source"\`` to `marketplaceEntry` (v1.go) and `catalogEntry` (plugins.go).

- [ ] **Step 2: Copy it in the build loops** — `entry.Source = rp.Source` in both `handleV1MarketplaceCatalog` and `handleListPlugins`.

- [ ] **Step 3: Update `v1_test.go::TestMarketplaceEntryTypeRoundTrips`** — set `rp.Source = registry.SourceInfo{URL: "u", Publisher: "OpenCapital", Verified: true}`; assert `got["source"].(map[string]any)["verified"] == true`.

- [ ] **Step 4: Run tests**

Run: `cd services/control-plane && go test ./internal/httpapi/ -run TestMarketplaceEntry -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/httpapi/
git commit -m "feat(httpapi): source-qualify marketplace + catalog entries"
```

---

### Task 14: `/v1/sources` CRUD

**Files:**
- Create: `services/control-plane/internal/httpapi/sources.go`, `sources_test.go`
- Modify: `services/control-plane/internal/httpapi/httpapi.go`

- [ ] **Step 1: Write `sources.go`**

```go
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/portfolio-management/control-plane/internal/manifest"
)

type sourceDTO struct {
	ManifestURL string `json:"manifest_url"`
	Publisher   string `json:"publisher"`
	Enabled     bool   `json:"enabled"`
}

type addSourceRequest struct {
	ManifestURL string `json:"manifest_url"`
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.store.ListPluginSources(ctx)
	if err != nil {
		s.logger.Error("sources: list", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	out := make([]sourceDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, sourceDTO{p.ManifestURL, p.Publisher, p.Enabled})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	var req addSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ManifestURL == "" {
		http.Error(w, "manifest_url required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	m, err := manifest.NewPluginClient(req.ManifestURL, nil, manifest.DefaultTTL, s.logger).Fetch(ctx)
	if err != nil {
		http.Error(w, "manifest unreachable or invalid: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.store.CreatePluginSource(ctx, req.ManifestURL, m.Publisher); err != nil {
		http.Error(w, "source already added or store error", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusCreated, sourceDTO{req.ManifestURL, m.Publisher, true})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("manifest_url")
	if url == "" {
		http.Error(w, "manifest_url query param required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	deleted, err := s.store.DeletePluginSource(ctx, url)
	if err != nil {
		s.logger.Error("sources: delete", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, "source not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Note: blocking deletion when an installed plugin came from the source needs a `source_url` column on `plugin_installs` (installs are official-only / id-keyed in v0). Recorded in Deferred.

- [ ] **Step 2: Add routes** in `httpapi.go` after the marketplace routes (~line 131)

```go
	// Federated plugin sources (desktop shell). Kinde-authenticated.
	mux.HandleFunc("GET /v1/sources", s.requireKindeSession(s.handleListSources))
	mux.HandleFunc("POST /v1/sources", s.requireKindeSession(s.handleAddSource))
	mux.HandleFunc("DELETE /v1/sources", s.requireKindeSession(s.handleDeleteSource))
```

- [ ] **Step 3: Write validation tests**

```go
package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/portfolio-management/control-plane/internal/config"
)

func TestAddSourceRejectsEmpty(t *testing.T) {
	s := &Server{cfg: config.Config{}, logger: slog.Default()}
	rr := httptest.NewRecorder()
	s.handleAddSource(rr, httptest.NewRequest(http.MethodPost, "/v1/sources", strings.NewReader(`{}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rr.Code)
	}
}

func TestDeleteSourceRequiresURL(t *testing.T) {
	s := &Server{cfg: config.Config{}, logger: slog.Default()}
	rr := httptest.NewRecorder()
	s.handleDeleteSource(rr, httptest.NewRequest(http.MethodDelete, "/v1/sources", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rr.Code)
	}
}
```

- [ ] **Step 4: Run tests**

Run: `cd services/control-plane && go test ./internal/httpapi/ -run 'TestAddSource|TestDeleteSource' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/control-plane/internal/httpapi/sources.go services/control-plane/internal/httpapi/sources_test.go services/control-plane/internal/httpapi/httpapi.go
git commit -m "feat(httpapi): /v1/sources list/add/delete endpoints"
```

---

## Phase G — CI cleanup

### Task 15: Drop the central plugin-promote workflows

The promotion system reconciled the trusted GHCR namespace to the old
`plugins.json` `.plugins` map (versioned trusted set). The new `plugins.json` is a
pointer list with no version/trusted-set semantics, so promotion is obsolete:
plugins publish to their own namespace and OpenCapital curates the list by PR
review. (Trust note: this removes the automated cosign-verify-before-trust gate;
manual `plugins.json` review replaces it, and an automated control-plane-side
signature verify is a future hook — spec §10.)

**Files:**
- Delete: `.github/workflows/plugin-promote-check.yml`
- Delete: `.github/workflows/plugin-promote-reconcile.yml`
- Delete: `.github/actions/plugin-promote/action.yml`, `.github/actions/plugin-promote/reconcile.sh`
- Delete: `.github/plugins/signers.yaml`

- [ ] **Step 1: Confirm nothing else references them**

Run: `grep -rn "plugin-promote\|signers.yaml\|PLUGIN_PROMOTE_TOKEN" .github/ ':!.github/skills/'`
Expected: only the four files above (and this is being deleted). If any OTHER
workflow references them, stop and reconcile before deleting.

- [ ] **Step 2: Delete the files**

Run:
```bash
git rm .github/workflows/plugin-promote-check.yml \
       .github/workflows/plugin-promote-reconcile.yml \
       .github/actions/plugin-promote/action.yml \
       .github/actions/plugin-promote/reconcile.sh \
       .github/plugins/signers.yaml
```

- [ ] **Step 3: Verify remaining workflows are unaffected**

Run: `grep -rln "plugins.json\|plugins-staging\|REGISTRY_NAMESPACE" .github/workflows/`
Expected: no matches (`opencapital-release.yml`, `images.yml`, `ci.yml`,
`opencapital-cache-warm.yml` do not depend on the dropped promotion).

- [ ] **Step 4: Commit**

```bash
git commit -m "ci: drop central plugin-promote (per-plugin manifests own versions)"
```

---

## Final verification

- [ ] **Build + test the whole service**

Run: `cd services/control-plane && go build ./... && go test ./...`
Expected: all PASS.

- [ ] **Build the desktop shell**

Run: `cd opencapital-app/src-tauri && cargo build`
Expected: PASS.

---

## Deferred to follow-up plans

- **Desktop Sources UI** (Plan 2): `SourcesView.tsx`, nav entry, Tauri commands `get_sources`/`add_source`/`remove_source`, card badge from `entry.source`, third-party install warning. Explore already mapped exact insertion points.
- **Block source deletion when an installed plugin came from it** — needs a `source_url` column on `plugin_installs`.
- **Per-source auth / private registries**, **signature verification for third-party**, **digest pinning** — spec §10/§11 future hardening.
