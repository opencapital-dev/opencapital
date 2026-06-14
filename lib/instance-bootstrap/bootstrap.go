// Package bootstrap renders Grafana plugin provisioning YAML for one
// single-org instance. Every Grafana process under v8 (desktop Tauri
// shell or cloud single-tenant container) runs an instance-bootstrap
// step before grafana-server: it asks the control plane which plugins
// are installed for the instance's org, then writes one provisioning
// YAML per plugin into Grafana's provisioning directory so grafana-server
// picks them up at boot.
//
// This package is the reusable logic. cmd/instance-bootstrap wires it up
// as a CLI for use from an entrypoint script.
package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is what an instance hands instance-bootstrap. Every field is
// fixed per instance and supplied via env at deploy time.
type Config struct {
	// OrgID is the control-plane org this instance belongs to (UUID).
	OrgID string

	// ControlPlaneURL is the base URL where the control plane lives.
	// Used for the bootstrap fetch.
	ControlPlaneURL string

	// BootstrapToken is the operator-shared secret the control plane's
	// /v1/internal/orgs/* endpoint accepts. v0: same value as
	// AdminBootstrapToken; Phase 3 introduces per-instance tokens.
	BootstrapToken string

	// ProvisioningDir is Grafana's provisioning directory. YAMLs are
	// written under ProvisioningDir/plugins/. Typically
	// /etc/grafana/provisioning on the cloud container; the desktop
	// shell picks a per-user path.
	ProvisioningDir string

	// PluginControlPlaneURL is the URL the plugin itself uses to reach
	// the control plane. May differ from ControlPlaneURL when control
	// plane is reachable on a different name from inside the Grafana
	// container vs from the bootstrap step.
	PluginControlPlaneURL string

	// PluginGatewayURL is the URL plugins use to POST data + tombstones.
	PluginGatewayURL string

	// PluginReadGatewayURL is the URL the core-datasource datasource posts metric
	// queries to (the read-gateway). Rendered into the core-datasource datasource
	// jsonData as `readGatewayUrl`.
	PluginReadGatewayURL string

	// PluginComputeURL is the loopback URL of the local Python compute sidecar
	// both datasource backends post {source, jwt, window} to. Rendered into the
	// core-datasource and plugindata datasource jsonData as `computeUrl`.
	PluginComputeURL string

	// PluginOTLPEndpoint is the OTel collector plugins ship spans to.
	PluginOTLPEndpoint string

	// InstanceTokenURL is the loopback endpoint where the instance serves
	// the current short-lived instance token (Option A). Rendered into each
	// app plugin's jsonData as `instanceTokenUrl`; pluginclient fetches the
	// token there and presents it as the /jwt/mint bearer. On desktop the
	// Tauri shell serves it; the cloud container serves it from its sidecar.
	InstanceTokenURL string

	// PluginRisingWaveHost/Port are how plugins reach RisingWave's pg-wire.
	// Rendered into app jsonData (Grafana sanitizes the plugin env, so this
	// can't travel as a process env var). On desktop = localhost + the host
	// port; in compose = the service name.
	PluginRisingWaveHost string
	PluginRisingWavePort string

	// GrafanaURL is the base URL of the local Grafana instance, used by the
	// post-start library-panels phase (e.g. http://localhost:3000).
	GrafanaURL string

	// GrafanaWebAuthUser, when non-empty, sends X-WEBAUTH-USER: <user> on
	// every Grafana API request. Used on desktop where auth.proxy lets any
	// header user become admin. Takes priority over Basic when set.
	GrafanaWebAuthUser string

	// GrafanaBasicUser / GrafanaBasicPassword send HTTP Basic credentials.
	// Used in the cloud container where the admin secret is available.
	GrafanaBasicUser     string
	GrafanaBasicPassword string

	// PluginStateDir is the writable root for per-(plugin,org) SQLite, rendered
	// into every plugin's jsonData as `pluginsRoot`. Required on the desktop:
	// Grafana sanitizes the plugin subprocess env, so a PLUGINS_ROOT env on
	// grafana-server never reaches the plugin, and pluginclient's
	// /var/lib/plugins default is unwritable on a laptop. Empty =>
	// pluginclient env/default fallback (fine in the container).
	PluginStateDir string

	// Pins maps plugin_id -> version for explicit local version overrides.
	// Absent or empty value falls back to the latest validated version.
	Pins map[string]string `json:"pins,omitempty"`

	// Platform is the host "<os>-<arch>" tag (e.g. "darwin-arm64") used to
	// select which artifact to download. Empty defaults to the running
	// host's GOOS-GOARCH.
	Platform string

	// PluginCacheDir is the machine-wide, content-addressed cache where
	// extracted plugin binaries live, keyed by <plugin_id>/<version>/
	// <platform>. Immutable per version; deduplicated across orgs.
	PluginCacheDir string

	// PluginsDir is the Grafana plugins directory for this instance. The
	// reconciler symlinks <PluginsDir>/<grafana_slug> -> the cache entry so
	// grafana-server loads the plugin.
	PluginsDir string

	// HTTPTimeout caps the bootstrap fetch + each artifact download.
	// Optional; defaults to 10s for the fetch, 5m for downloads.
	HTTPTimeout time.Duration

	// ProgressW receives NDJSON progress events (one JSON object per line)
	// for the desktop shell to render. Defaults to os.Stdout.
	ProgressW io.Writer
}

// Plugin mirrors the control plane's instancePluginEntry response shape.
type Plugin struct {
	PluginID      string `json:"plugin_id"`
	GrafanaSlug   string `json:"grafana_slug"`
	PlatformToken string `json:"platform_token"`
	Required      bool   `json:"required"`
	// Type is the Grafana plugin kind (app/datasource/panel), sourced from
	// plugin.json at publish. Render (Task B2) switches on it to decide the
	// provisioning shape.
	Type string `json:"type"`
	// PlatformPlugin marks an infrastructure plugin (e.g. the core-datasource
	// datasource). Its binary is installed like any plugin, but its
	// provisioning is a Grafana datasource the instance renders itself, not
	// an app YAML — so Render skips it.
	PlatformPlugin bool   `json:"platform_plugin,omitempty"`
	Version        string `json:"version,omitempty"`
	// QueryEntities are the plugin's declared queryable entities, part of the
	// control-plane response contract. The compute sidecar (via the read-gateway)
	// consumes them now; bootstrap only decodes them. Kept as []map[string]any so
	// bootstrap doesn't re-type the declaration.
	QueryEntities []map[string]any `json:"query_entities,omitempty"`
	Artifact      *Artifact        `json:"artifact,omitempty"`
}

// Artifact is the host-platform download the control plane selected for
// this plugin (via the ?platform= query). Nil when no artifact is
// published for the requesting platform.
type Artifact struct {
	DownloadURL string `json:"download_url"`
	Sha256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
}

// VersionStatus mirrors the control-plane version-list element: a version tag
// and whether it is validated (promoted) or preview (staging-only).
type VersionStatus struct {
	Version   string `json:"version"`
	Validated bool   `json:"validated"`
}

// ParsePins decodes the PLUGIN_PINS env value (a JSON object plugin_id->version)
// into a pin map. Empty or malformed input yields nil (no pins -> latest validated).
func ParsePins(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// resolveTarget picks the version to run: an explicit local pin if present,
// otherwise the latest validated version (versions arrive newest-first).
// Returns "" when there is no pin and no validated version (caller skips).
func resolveTarget(pluginID string, pins map[string]string, versions []VersionStatus) string {
	if v, ok := pins[pluginID]; ok && v != "" {
		return v
	}
	for _, vs := range versions {
		if vs.Validated {
			return vs.Version
		}
	}
	return ""
}

// Run executes the full reconcile: fetch the org's plugin set, install
// each plugin's host-platform binary from the artifact host (download +
// verify + extract + symlink), prune plugins no longer desired, and render
// provisioning YAML. Returns the number of plugins provisioned.
func Run(ctx context.Context, cfg Config) (int, error) {
	if err := validate(cfg); err != nil {
		return 0, err
	}
	plugins, err := Fetch(ctx, cfg)
	if err != nil {
		return 0, fmt.Errorf("fetch installed plugins: %w", err)
	}
	provisionable, err := installAll(ctx, cfg, plugins)
	if err != nil {
		return 0, err
	}
	if err := prune(cfg, plugins); err != nil {
		return 0, fmt.Errorf("prune plugins: %w", err)
	}
	if err := Render(cfg, provisionable); err != nil {
		return 0, fmt.Errorf("render provisioning: %w", err)
	}
	if err := provisionDashboards(cfg, provisionable); err != nil {
		return 0, fmt.Errorf("provision dashboards: %w", err)
	}
	return len(provisionable), nil
}

// getJSON performs an authenticated GET against url, decoding the JSON
// response into out. Reused by Fetch and its version/artifact helpers.
func getJSON(ctx context.Context, cfg Config, url string, out any) error {
	timeout := cfg.HTTPTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.BootstrapToken)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control plane %s: %s", resp.Status, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response from %s: %w", url, err)
	}
	return nil
}

// fetchVersions returns the version list for a plugin from the control plane.
func fetchVersions(ctx context.Context, cfg Config, id string) ([]VersionStatus, error) {
	url := fmt.Sprintf("%s/v1/internal/plugins/%s/versions", cfg.ControlPlaneURL, id)
	var body struct {
		Versions []VersionStatus `json:"versions"`
	}
	if err := getJSON(ctx, cfg, url, &body); err != nil {
		return nil, err
	}
	return body.Versions, nil
}

// fetchArtifact returns the artifact descriptor for a specific plugin version
// and platform from the control plane.
func fetchArtifact(ctx context.Context, cfg Config, id, version, platform string) (*Artifact, error) {
	url := fmt.Sprintf("%s/v1/internal/plugins/%s/versions/%s/artifact?platform=%s",
		cfg.ControlPlaneURL, id, version, platform)
	var art Artifact
	if err := getJSON(ctx, cfg, url, &art); err != nil {
		return nil, err
	}
	return &art, nil
}

// Fetch calls GET /v1/internal/orgs/{org_id}/plugins?platform=<p> on the
// control plane with the bootstrap bearer, then resolves each plugin's version
// locally via resolveTarget and fetches its artifact descriptor.
func Fetch(ctx context.Context, cfg Config) ([]Plugin, error) {
	grantURL := fmt.Sprintf("%s/v1/internal/orgs/%s/plugins?platform=%s",
		cfg.ControlPlaneURL, cfg.OrgID, hostPlatform(cfg))
	var plugins []Plugin
	if err := getJSON(ctx, cfg, grantURL, &plugins); err != nil {
		return nil, fmt.Errorf("decode plugins response: %w", err)
	}
	platform := hostPlatform(cfg)
	for i := range plugins {
		vs, err := fetchVersions(ctx, cfg, plugins[i].PluginID)
		if err != nil {
			return nil, fmt.Errorf("versions %s: %w", plugins[i].PluginID, err)
		}
		target := resolveTarget(plugins[i].PluginID, cfg.Pins, vs)
		if target == "" {
			continue
		}
		plugins[i].Version = target
		art, err := fetchArtifact(ctx, cfg, plugins[i].PluginID, target, platform)
		if err != nil {
			return nil, fmt.Errorf("artifact %s@%s: %w", plugins[i].PluginID, target, err)
		}
		plugins[i].Artifact = art
	}
	return plugins, nil
}

// Render writes one provisioning YAML per plugin into
// ProvisioningDir/plugins/<plugin_id>.yaml. Returns the first error
// encountered; partial writes are left in place so the operator can
// inspect the failure (instance-bootstrap is run before grafana-server
// so a failed YAML is caught at boot, not at runtime).
func Render(cfg Config, plugins []Plugin) error {
	outDir := filepath.Join(cfg.ProvisioningDir, "plugins")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	// Drop YAMLs for plugins no longer provisionable (e.g. uninstalled). A
	// stale app YAML referencing a missing plugin makes Grafana fail
	// provisioning on boot, so the rendered set must match `plugins` exactly.
	if err := pruneProvisioning(outDir, plugins); err != nil {
		return fmt.Errorf("prune provisioning: %w", err)
	}
	for _, p := range plugins {
		if p.GrafanaSlug == "" {
			// Control plane is supposed to filter these out; defend
			// anyway so a manifest gap doesn't render a junk YAML.
			continue
		}
		// `type` decides the provisioning KIND (datasource vs app vs none);
		// `platform_plugin` decides how much platform wiring a datasource gets
		// (full vs minimal). They are orthogonal — both honored here.
		switch p.Type {
		case "datasource":
			if p.PlatformPlugin {
				// Datasource/infra plugin (the core-datasource datasource): the
				// binary is installed above; here we render its Grafana
				// *datasource* provisioning. It forwards metric queries to the
				// read-gateway and authenticates with its per-org platform
				// token, so it needs the same identity jsonData as the app
				// plugins plus readGatewayUrl.
				if err := renderDatasource(cfg, p, plugins); err != nil {
					return err
				}
				continue
			}
			// Generic community datasource (platform_plugin=false): provisioned
			// as a bare editable datasource with no platform wiring.
			if err := renderMinimalDatasource(cfg, p); err != nil {
				return err
			}
		case "app":
			if err := renderApp(cfg, outDir, p); err != nil {
				return err
			}
		case "panel":
			// Panels only need to load; no provisioning entry.
			continue
		default:
			// Defensive: a plugin whose footprint predates the type field, or
			// an unknown kind. Skip provisioning rather than crash. Emit on the
			// progress stream so the desktop shell sees the skip (a raw stderr
			// write would bypass cfg.ProgressW).
			emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "skipped", Detail: fmt.Sprintf("unknown grafana type %q", p.Type)})
			continue
		}
	}
	if err := writeUnsignedPluginsList(cfg, plugins); err != nil {
		return err
	}
	return nil
}

// writeUnsignedPluginsList writes a sidecar file at
// ProvisioningDir/unsigned-plugins listing the Grafana slugs of all installed
// plugins, comma-joined and sorted, so the launch shell can build Grafana's
// allow_loading_unsigned_plugins dynamically. All plugin types (app,
// datasource, panel) are included because every plugin in this project loads
// unsigned. Plugins with an empty GrafanaSlug are skipped; duplicates are
// deduplicated. Empty set writes an empty file (zero bytes).
func writeUnsignedPluginsList(cfg Config, plugins []Plugin) error {
	seen := make(map[string]struct{}, len(plugins))
	for _, p := range plugins {
		if p.GrafanaSlug != "" {
			seen[p.GrafanaSlug] = struct{}{}
		}
	}
	slugs := make([]string, 0, len(seen))
	for s := range seen {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	content := strings.Join(slugs, ",")
	path := filepath.Join(cfg.ProvisioningDir, "unsigned-plugins")
	// 0o600: matches the provisioning YAML file-permission convention; the
	// file sits at the provisioning root where Grafana won't scan it, so the
	// tighter permission is purely defensive.
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write unsigned-plugins sidecar: %w", err)
	}
	return nil
}

// pruneProvisioning removes <plugin_id>.yaml files in outDir whose plugin is
// not in the desired (provisionable) set. Mirrors prune() for symlinks: only
// the .yaml files this reconciler writes are considered.
func pruneProvisioning(outDir string, desired []Plugin) error {
	want := make(map[string]bool, len(desired))
	for _, p := range desired {
		// Only app plugins write into the plugins/ dir; datasources (platform
		// or minimal) and panels do not, so they must not be retained here.
		if p.GrafanaSlug != "" && p.Type == "app" {
			want[p.PluginID+".yaml"] = true
		}
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") || want[name] {
			continue
		}
		if err := os.Remove(filepath.Join(outDir, name)); err != nil {
			return fmt.Errorf("remove stale provisioning %s: %w", name, err)
		}
	}
	return nil
}

// provisioningDoc matches Grafana's plugins provisioning schema.
type provisioningDoc struct {
	APIVersion int               `yaml:"apiVersion"`
	Apps       []provisioningApp `yaml:"apps,omitempty"`
}

type provisioningApp struct {
	Type           string            `yaml:"type"`
	OrgID          int               `yaml:"org_id"`
	Disabled       bool              `yaml:"disabled"`
	JSONData       map[string]any    `yaml:"jsonData"`
	SecureJSONData map[string]string `yaml:"secureJsonData"`
}

// datasourceDoc matches Grafana's datasource provisioning schema.
type datasourceDoc struct {
	APIVersion  int               `yaml:"apiVersion"`
	Datasources []datasourceEntry `yaml:"datasources"`
}

type datasourceEntry struct {
	Name           string            `yaml:"name"`
	UID            string            `yaml:"uid"`
	Type           string            `yaml:"type"`
	Access         string            `yaml:"access"`
	IsDefault      bool              `yaml:"isDefault"`
	Editable       bool              `yaml:"editable"`
	JSONData       map[string]any    `yaml:"jsonData"`
	SecureJSONData map[string]string `yaml:"secureJsonData"`
}

// renderApp writes an app plugin's Grafana provisioning YAML into
// ProvisioningDir/plugins/<plugin_id>.yaml.
func renderApp(cfg Config, outDir string, p Plugin) error {
	doc := provisioningDoc{
		APIVersion: 1,
		Apps: []provisioningApp{{
			Type:     p.GrafanaSlug,
			OrgID:    1,
			Disabled: false,
			JSONData: map[string]any{
				"pluginId":                 p.PluginID,
				"orgId":                    cfg.OrgID,
				"controlPlaneUrl":          cfg.PluginControlPlaneURL,
				"gatewayUrl":               cfg.PluginGatewayURL,
				"otelExporterOtlpEndpoint": cfg.PluginOTLPEndpoint,
				"instanceTokenUrl":         cfg.InstanceTokenURL,
				"risingwaveHost":           cfg.PluginRisingWaveHost,
				"risingwavePort":           cfg.PluginRisingWavePort,
				"pluginsRoot":              cfg.PluginStateDir,
				"readGatewayUrl":           cfg.PluginReadGatewayURL,
			},
			SecureJSONData: map[string]string{
				"platformToken": p.PlatformToken,
			},
		}},
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal yaml for %s: %w", p.PluginID, err)
	}
	// 0o600: provisioning files carry platform_tokens (secureJsonData is
	// plaintext until Grafana reads + re-encrypts at start).
	path := filepath.Join(outDir, p.PluginID+".yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// renderDatasource writes a platform plugin's Grafana datasource provisioning
// YAML into ProvisioningDir/datasources/<plugin_id>.yaml. The uid matches the
// plugin id so dashboards can reference it by a stable name. jsonData carries
// the compute sidecar URL plus the same identity fields the app plugins get; the
// per-org platform token rides in secureJsonData, alongside pluginTokens — a
// JSON-encoded map of every installed plugin's platformToken so the datasource
// backend can open foreign encrypted SQLites.
func renderDatasource(cfg Config, p Plugin, plugins []Plugin) error {
	tokenMap := make(map[string]string, len(plugins))
	for _, pl := range plugins {
		if pl.PlatformToken != "" {
			tokenMap[pl.PluginID] = pl.PlatformToken
		}
	}
	tokensJSON, err := json.Marshal(tokenMap)
	if err != nil {
		return fmt.Errorf("marshal pluginTokens for %s: %w", p.PluginID, err)
	}
	doc := datasourceDoc{
		APIVersion: 1,
		Datasources: []datasourceEntry{{
			Name:      p.PluginID,
			UID:       p.PluginID,
			Type:      p.GrafanaSlug,
			Access:    "proxy",
			IsDefault: false,
			Editable:  true,
			JSONData: map[string]any{
				"computeUrl":       cfg.PluginComputeURL,
				"pluginId":         p.PluginID,
				"orgId":            cfg.OrgID,
				"controlPlaneUrl":  cfg.PluginControlPlaneURL,
				"gatewayUrl":       cfg.PluginGatewayURL,
				"instanceTokenUrl": cfg.InstanceTokenURL,
				"pluginsRoot":      cfg.PluginStateDir,
				// Plugins install (symlink) dir: the datasource resolves metric
				// refs `pluginID/metric` to
				// <pluginsInstallDir>/<pluginID>/library-panels/<metric>.py.
				"pluginsInstallDir": cfg.PluginsDir,
			},
			SecureJSONData: map[string]string{
				"platformToken": p.PlatformToken,
				"pluginTokens":  string(tokensJSON),
			},
		}},
	}
	return writeDatasourceDoc(cfg, p.PluginID, doc)
}

// renderMinimalDatasource provisions a generic community datasource (one we do
// not specially configure). It carries only the bare identity fields Grafana
// needs to register an editable datasource — no jsonData/secureJsonData.
func renderMinimalDatasource(cfg Config, p Plugin) error {
	doc := datasourceDoc{
		APIVersion: 1,
		Datasources: []datasourceEntry{{
			Name:      p.PluginID,
			UID:       p.PluginID,
			Type:      p.GrafanaSlug,
			Access:    "proxy",
			IsDefault: false,
			Editable:  true,
		}},
	}
	return writeDatasourceDoc(cfg, p.PluginID, doc)
}

// writeDatasourceDoc marshals a datasource provisioning doc and writes it to
// ProvisioningDir/datasources/<plugin_id>.yaml. Shared tail for the platform
// and minimal datasource renderers.
func writeDatasourceDoc(cfg Config, pluginID string, doc datasourceDoc) error {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal datasource yaml for %s: %w", pluginID, err)
	}
	dsDir := filepath.Join(cfg.ProvisioningDir, "datasources")
	if err := os.MkdirAll(dsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dsDir, err)
	}
	// 0o600: may carry the platform token (plaintext until Grafana encrypts).
	path := filepath.Join(dsDir, pluginID+".yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func validate(cfg Config) error {
	if cfg.OrgID == "" {
		return errors.New("OrgID is required")
	}
	if cfg.ControlPlaneURL == "" {
		return errors.New("ControlPlaneURL is required")
	}
	if cfg.BootstrapToken == "" {
		return errors.New("BootstrapToken is required")
	}
	if cfg.ProvisioningDir == "" {
		return errors.New("ProvisioningDir is required")
	}
	if cfg.PluginControlPlaneURL == "" {
		return errors.New("PluginControlPlaneURL is required")
	}
	if cfg.PluginGatewayURL == "" {
		return errors.New("PluginGatewayURL is required")
	}
	if cfg.PluginCacheDir == "" {
		return errors.New("PluginCacheDir is required")
	}
	if cfg.PluginsDir == "" {
		return errors.New("PluginsDir is required")
	}
	return nil
}

// validateLibraryPanels checks the config the post-start library-panels phase
// needs: GrafanaURL (an empty value builds a relative base URL that fails
// opaquely) plus the reconcile fields Fetch depends on.
func validateLibraryPanels(cfg Config) error {
	if cfg.GrafanaURL == "" {
		return errors.New("GrafanaURL is required")
	}
	if cfg.OrgID == "" {
		return errors.New("OrgID is required")
	}
	if cfg.ControlPlaneURL == "" {
		return errors.New("ControlPlaneURL is required")
	}
	if cfg.BootstrapToken == "" {
		return errors.New("BootstrapToken is required")
	}
	return nil
}
