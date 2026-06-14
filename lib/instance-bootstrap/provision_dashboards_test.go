package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func dashCfg(t *testing.T) Config {
	t.Helper()
	return Config{
		OrgID:                 testOrgID,
		ControlPlaneURL:       "http://unused",
		BootstrapToken:        "tok",
		ProvisioningDir:       t.TempDir(),
		PluginControlPlaneURL: "http://cp",
		PluginGatewayURL:      "http://gw",
		Platform:              "linux-amd64",
		PluginCacheDir:        t.TempDir(),
		PluginsDir:            t.TempDir(),
	}
}

// seedPlugin creates a fake installed-plugin tree under cfg.PluginsDir:
//
//	<PluginsDir>/<slug>/dashboards/<relpath>
//
// relpath may contain subdirs (e.g. "risk/overview.json") to exercise nested
// bundle dashboards.
func seedPlugin(t *testing.T, cfg Config, slug, relpath, content string) {
	t.Helper()
	file := filepath.Join(cfg.PluginsDir, slug, "dashboards", relpath)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("seed plugin dir: %v", err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatalf("seed dashboard file: %v", err)
	}
}

// pluginDashPath is the dest content subroot a plugin's dashboards land under:
// <ProvisioningDir>/dashboards/plugins/<plugin_id>/<rel...>.
func pluginDashPath(cfg Config, pluginID string, rel ...string) string {
	parts := append([]string{cfg.ProvisioningDir, "dashboards", "plugins", pluginID}, rel...)
	return filepath.Join(parts...)
}

// parsedProvider reads and unmarshals the provider YAML from <ProvisioningDir>/dashboards/plugin-dashboards.yaml.
func parsedProvider(t *testing.T, cfg Config) dashboardProviderDoc {
	t.Helper()
	path := filepath.Join(cfg.ProvisioningDir, "dashboards", "plugin-dashboards.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read provider yaml: %v", err)
	}
	var doc dashboardProviderDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal provider yaml: %v", err)
	}
	return doc
}

func TestProvisionDashboards_CopiesFilesAndWritesProviderYAML(t *testing.T) {
	cfg := dashCfg(t)
	const dashContent = `{"title":"My Dashboard","uid":"abc"}`
	seedPlugin(t, cfg, "pm-myplugin-app", "overview.json", dashContent)

	plugins := []Plugin{
		{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"},
	}
	if err := provisionDashboards(cfg, plugins); err != nil {
		t.Fatalf("provisionDashboards: %v", err)
	}

	// Copied file must exist at <ProvisioningDir>/dashboards/plugins/<plugin_id>/<file>.
	dest := pluginDashPath(cfg, "myplugin", "overview.json")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read copied dashboard: %v", err)
	}
	if string(data) != dashContent {
		t.Errorf("dashboard content = %q, want %q", string(data), dashContent)
	}

	// Provider YAML must exist and have the correct settings.
	doc := parsedProvider(t, cfg)
	if doc.APIVersion != 1 {
		t.Errorf("apiVersion = %d, want 1", doc.APIVersion)
	}
	if len(doc.Providers) != 1 {
		t.Fatalf("len(providers) = %d, want 1", len(doc.Providers))
	}
	p := doc.Providers[0]
	if p.AllowUIUpdates {
		t.Errorf("allowUiUpdates = true, want false")
	}
	if !p.DisableDeletion {
		t.Errorf("disableDeletion = false, want true")
	}
	if !p.Options.FoldersFromFilesStructure {
		t.Errorf("foldersFromFilesStructure = false, want true")
	}
	contentRoot := filepath.Join(cfg.ProvisioningDir, "dashboards", "plugins")
	if p.Options.Path != contentRoot {
		t.Errorf("options.path = %q, want %q", p.Options.Path, contentRoot)
	}
	if p.OrgID != 1 {
		t.Errorf("orgId = %d, want 1", p.OrgID)
	}
	if p.Type != "file" {
		t.Errorf("type = %q, want file", p.Type)
	}
}

func TestProvisionDashboards_PrunesRemovedPlugin(t *testing.T) {
	cfg := dashCfg(t)
	seedPlugin(t, cfg, "pm-myplugin-app", "overview.json", `{"uid":"x"}`)

	plugins := []Plugin{
		{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"},
	}
	if err := provisionDashboards(cfg, plugins); err != nil {
		t.Fatalf("provisionDashboards first run: %v", err)
	}

	// Verify file was copied.
	dest := pluginDashPath(cfg, "myplugin", "overview.json")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected copied dashboard after first run: %v", err)
	}

	// Second run with an empty desired set (plugin uninstalled).
	if err := provisionDashboards(cfg, nil); err != nil {
		t.Fatalf("provisionDashboards prune run: %v", err)
	}

	// Plugin subdir must be gone.
	pluginDir := pluginDashPath(cfg, "myplugin")
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Errorf("expected myplugin dir pruned, stat err = %v", err)
	}

	// Provider YAML must still exist.
	yamlPath := filepath.Join(cfg.ProvisioningDir, "dashboards", "plugin-dashboards.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Errorf("provider yaml should survive prune: %v", err)
	}
}

func TestProvisionDashboards_AppPluginNoDashboardsDir(t *testing.T) {
	// An app plugin that ships no dashboards/ dir must produce no error and
	// no subdir under <ProvisioningDir>/dashboards.
	cfg := dashCfg(t)
	// Create the slug dir but no dashboards/ subdir inside it.
	if err := os.MkdirAll(filepath.Join(cfg.PluginsDir, "pm-nodash-app"), 0o755); err != nil {
		t.Fatal(err)
	}

	plugins := []Plugin{
		{PluginID: "nodash", GrafanaSlug: "pm-nodash-app", Type: "app"},
	}
	if err := provisionDashboards(cfg, plugins); err != nil {
		t.Fatalf("provisionDashboards: %v", err)
	}

	// No subdir created.
	if _, err := os.Stat(pluginDashPath(cfg, "nodash")); !os.IsNotExist(err) {
		t.Errorf("expected no subdir for plugin with no dashboards dir; stat err = %v", err)
	}

	// Provider YAML still written (harmless).
	if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "dashboards", "plugin-dashboards.yaml")); err != nil {
		t.Errorf("provider yaml should be written even when no dashboards present: %v", err)
	}
}

func TestProvisionDashboards_NonAppPluginsSkipped(t *testing.T) {
	cfg := dashCfg(t)
	// Seed files for a datasource and panel plugin — they must not be copied.
	seedPlugin(t, cfg, "pm-myds-datasource", "dash.json", `{}`)
	seedPlugin(t, cfg, "pm-mypanel-panel", "dash.json", `{}`)

	plugins := []Plugin{
		{PluginID: "myds", GrafanaSlug: "pm-myds-datasource", Type: "datasource"},
		{PluginID: "mypanel", GrafanaSlug: "pm-mypanel-panel", Type: "panel"},
	}
	if err := provisionDashboards(cfg, plugins); err != nil {
		t.Fatalf("provisionDashboards: %v", err)
	}

	for _, id := range []string{"myds", "mypanel"} {
		if _, err := os.Stat(pluginDashPath(cfg, id)); !os.IsNotExist(err) {
			t.Errorf("expected no subdir for non-app plugin %s; stat err = %v", id, err)
		}
	}
}

// Fix 2: a dashboard that vanishes from the bundle on the next reconcile must be
// removed from the dest (Grafana only unprovisions when the JSON file disappears).
func TestProvisionDashboards_RemovesStaleDashboardOnReconcile(t *testing.T) {
	cfg := dashCfg(t)
	const slug = "pm-myplugin-app"
	seedPlugin(t, cfg, slug, "overview.json", `{"uid":"a"}`)
	seedPlugin(t, cfg, slug, "legacy.json", `{"uid":"b"}`)

	plugins := []Plugin{{PluginID: "myplugin", GrafanaSlug: slug, Type: "app"}}
	if err := provisionDashboards(cfg, plugins); err != nil {
		t.Fatalf("provisionDashboards first run: %v", err)
	}
	if _, err := os.Stat(pluginDashPath(cfg, "myplugin", "legacy.json")); err != nil {
		t.Fatalf("expected legacy.json after first run: %v", err)
	}

	// New bundle version drops legacy.json.
	if err := os.Remove(filepath.Join(cfg.PluginsDir, slug, "dashboards", "legacy.json")); err != nil {
		t.Fatalf("remove legacy from bundle: %v", err)
	}
	if err := provisionDashboards(cfg, plugins); err != nil {
		t.Fatalf("provisionDashboards reconcile run: %v", err)
	}

	if _, err := os.Stat(pluginDashPath(cfg, "myplugin", "legacy.json")); !os.IsNotExist(err) {
		t.Errorf("expected legacy.json removed on reconcile; stat err = %v", err)
	}
	if _, err := os.Stat(pluginDashPath(cfg, "myplugin", "overview.json")); err != nil {
		t.Errorf("expected overview.json to survive reconcile: %v", err)
	}
}

// Fix 3: a nested bundle dashboard must land at the matching nested dest path so
// foldersFromFilesStructure turns it into a nested Grafana folder.
func TestProvisionDashboards_NestedBundleDashboard(t *testing.T) {
	cfg := dashCfg(t)
	const nested = `{"uid":"risk"}`
	seedPlugin(t, cfg, "pm-myplugin-app", filepath.Join("risk", "overview.json"), nested)
	seedPlugin(t, cfg, "pm-myplugin-app", "notes.txt", "ignore me")

	plugins := []Plugin{{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"}}
	if err := provisionDashboards(cfg, plugins); err != nil {
		t.Fatalf("provisionDashboards: %v", err)
	}

	dest := pluginDashPath(cfg, "myplugin", "risk", "overview.json")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read nested dashboard: %v", err)
	}
	if string(data) != nested {
		t.Errorf("nested dashboard content = %q, want %q", string(data), nested)
	}

	// Non-.json sibling must be skipped.
	if _, err := os.Stat(pluginDashPath(cfg, "myplugin", "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("expected non-json file skipped; stat err = %v", err)
	}
}

// Fix 1 boundary: a sibling dir like <ProvisioningDir>/dashboards/json that the
// launcher owns must never be pruned or touched, because the subsystem's content
// root and prune scope are confined to dashboards/plugins.
func TestProvisionDashboards_DoesNotTouchSiblingDir(t *testing.T) {
	cfg := dashCfg(t)

	// Simulate the launcher's curated-dashboards dir.
	launcherDir := filepath.Join(cfg.ProvisioningDir, "dashboards", "json")
	if err := os.MkdirAll(launcherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	launcherFile := filepath.Join(launcherDir, "curated.json")
	if err := os.WriteFile(launcherFile, []byte(`{"uid":"curated"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	seedPlugin(t, cfg, "pm-myplugin-app", "overview.json", `{"uid":"a"}`)
	plugins := []Plugin{{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"}}

	// Run with the plugin, then reconcile with an empty set (which prunes).
	if err := provisionDashboards(cfg, plugins); err != nil {
		t.Fatalf("provisionDashboards: %v", err)
	}
	if err := provisionDashboards(cfg, nil); err != nil {
		t.Fatalf("provisionDashboards prune run: %v", err)
	}

	if _, err := os.Stat(launcherFile); err != nil {
		t.Errorf("launcher dashboards/json must be untouched: %v", err)
	}
}
