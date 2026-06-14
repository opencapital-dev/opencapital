package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	testOrgID = "00000000-0000-0000-0000-000000000001"
	testToken = "secret-bootstrap-token"
)

func validConfig(t *testing.T, controlPlaneURL string) Config {
	t.Helper()
	return Config{
		OrgID:                 testOrgID,
		ControlPlaneURL:       controlPlaneURL,
		BootstrapToken:        testToken,
		ProvisioningDir:       t.TempDir(),
		PluginControlPlaneURL: "http://control-plane:8080",
		PluginGatewayURL:      "http://gateway:8090",
		PluginReadGatewayURL:  "http://read-gateway:8095",
		PluginComputeURL:      "http://127.0.0.1:8799",
		PluginOTLPEndpoint:    "http://alloy:4317",
		Platform:              "linux-amd64",
		PluginCacheDir:        t.TempDir(),
		PluginsDir:            t.TempDir(),
	}
}

func TestValidateRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"missing OrgID", func(c *Config) { c.OrgID = "" }, "OrgID"},
		{"missing ControlPlaneURL", func(c *Config) { c.ControlPlaneURL = "" }, "ControlPlaneURL"},
		{"missing BootstrapToken", func(c *Config) { c.BootstrapToken = "" }, "BootstrapToken"},
		{"missing ProvisioningDir", func(c *Config) { c.ProvisioningDir = "" }, "ProvisioningDir"},
		{"missing PluginControlPlaneURL", func(c *Config) { c.PluginControlPlaneURL = "" }, "PluginControlPlaneURL"},
		{"missing PluginGatewayURL", func(c *Config) { c.PluginGatewayURL = "" }, "PluginGatewayURL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t, "http://localhost:0")
			tc.mod(&cfg)
			err := validate(cfg)
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error to mention %q, got %q", tc.want, err.Error())
			}
		})
	}
}

func TestFetchSendsBearerAndReturnsPlugins(t *testing.T) {
	grants := []Plugin{
		{PluginID: "core-app", GrafanaSlug: "slug-pa", PlatformToken: "tok-pa", Required: true, Type: "app"},
		{PluginID: "yfinance-app", GrafanaSlug: "slug-yf", PlatformToken: "tok-yf", Required: false, Type: "datasource"},
	}
	var gotAuth string
	var gotGrantPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotAuth == "" {
			gotAuth = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/plugins"):
			gotGrantPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(grants)
		case strings.Contains(r.URL.Path, "/core-app/versions/v1.0.0/artifact"):
			_ = json.NewEncoder(w).Encode(Artifact{DownloadURL: "http://x/pa.tgz", Sha256: "aaa", SizeBytes: 10})
		case strings.Contains(r.URL.Path, "/yfinance-app/versions/v2.0.0/artifact"):
			_ = json.NewEncoder(w).Encode(Artifact{DownloadURL: "http://x/yf.tgz", Sha256: "bbb", SizeBytes: 20})
		case strings.HasSuffix(r.URL.Path, "/plugins/core-app/versions"):
			_ = json.NewEncoder(w).Encode(map[string]any{"versions": []VersionStatus{{Version: "v1.0.0", Validated: true}}})
		case strings.HasSuffix(r.URL.Path, "/plugins/yfinance-app/versions"):
			_ = json.NewEncoder(w).Encode(map[string]any{"versions": []VersionStatus{{Version: "v2.0.0", Validated: true}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := validConfig(t, srv.URL)
	got, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want Bearer %s", gotAuth, testToken)
	}
	if gotGrantPath != "/v1/internal/orgs/"+testOrgID+"/plugins" {
		t.Errorf("grant path = %q", gotGrantPath)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2", len(got))
	}
	// Verify identity fields from the grant list are preserved.
	if got[0].PluginID != "core-app" || got[0].GrafanaSlug != "slug-pa" || got[0].PlatformToken != "tok-pa" {
		t.Errorf("plugin[0] identity = %+v", got[0])
	}
	// Verify version + artifact were resolved locally.
	if got[0].Version != "v1.0.0" {
		t.Errorf("plugin[0].Version = %q, want v1.0.0", got[0].Version)
	}
	if got[0].Artifact == nil || got[0].Artifact.DownloadURL != "http://x/pa.tgz" {
		t.Errorf("plugin[0].Artifact = %+v", got[0].Artifact)
	}
	if got[1].Version != "v2.0.0" {
		t.Errorf("plugin[1].Version = %q, want v2.0.0", got[1].Version)
	}
	if got[1].Artifact == nil || got[1].Artifact.DownloadURL != "http://x/yf.tgz" {
		t.Errorf("plugin[1].Artifact = %+v", got[1].Artifact)
	}
}

func TestFetchFailsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := validConfig(t, srv.URL)
	_, err := Fetch(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q should mention 401", err.Error())
	}
}

func TestRenderWritesOneYAMLPerPlugin(t *testing.T) {
	plugins := []Plugin{
		{PluginID: "core-app", GrafanaSlug: "core-app", PlatformToken: "tok-pa", Type: "app"},
		{PluginID: "yfinance-app", GrafanaSlug: "yfinance-app", PlatformToken: "tok-yf", Type: "app"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, p := range plugins {
		path := filepath.Join(cfg.ProvisioningDir, "plugins", p.PluginID+".yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc provisioningDoc
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("yaml unmarshal %s: %v", path, err)
		}
		if doc.APIVersion != 1 {
			t.Errorf("apiVersion = %d, want 1", doc.APIVersion)
		}
		if len(doc.Apps) != 1 {
			t.Fatalf("len(apps) = %d, want 1", len(doc.Apps))
		}
		app := doc.Apps[0]
		if app.Type != p.GrafanaSlug {
			t.Errorf("type = %q, want %q", app.Type, p.GrafanaSlug)
		}
		if app.OrgID != 1 {
			t.Errorf("org_id = %d, want 1", app.OrgID)
		}
		if app.JSONData["pluginId"] != p.PluginID {
			t.Errorf("pluginId = %v, want %s", app.JSONData["pluginId"], p.PluginID)
		}
		if app.JSONData["orgId"] != cfg.OrgID {
			t.Errorf("orgId = %v, want %s", app.JSONData["orgId"], cfg.OrgID)
		}
		if app.SecureJSONData["platformToken"] != p.PlatformToken {
			t.Errorf("platformToken = %v, want %s", app.SecureJSONData["platformToken"], p.PlatformToken)
		}
		// 0o600 because secureJsonData carries platform tokens until
		// Grafana reads them at boot.
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0o600", info.Mode().Perm())
		}
	}
}

func TestRenderSkipsPlatformPlugin(t *testing.T) {
	// A PlatformPlugin (e.g. core-datasource) has its binary installed but its
	// provisioning is a datasource the instance renders itself — Render must
	// not write an app YAML for it.
	plugins := []Plugin{
		{PluginID: "core-datasource", GrafanaSlug: "core-datasource", PlatformPlugin: true, Type: "datasource"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := filepath.Join(cfg.ProvisioningDir, "plugins", "core-datasource.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no app YAML for platform plugin; stat err = %v", err)
	}
}

func TestRenderStampsInstanceTokenURL(t *testing.T) {
	plugins := []Plugin{
		{PluginID: "core-app", GrafanaSlug: "core-app", PlatformToken: "tok", Type: "app"},
	}
	cfg := validConfig(t, "")
	cfg.InstanceTokenURL = "http://127.0.0.1:7666/instance-token"
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.ProvisioningDir, "plugins", "core-app.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc provisioningDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := doc.Apps[0].JSONData["instanceTokenUrl"]; got != cfg.InstanceTokenURL {
		t.Errorf("instanceTokenUrl = %v, want %s", got, cfg.InstanceTokenURL)
	}
	if got := doc.Apps[0].JSONData["readGatewayUrl"]; got != cfg.PluginReadGatewayURL {
		t.Errorf("readGatewayUrl = %v, want %s", got, cfg.PluginReadGatewayURL)
	}
}

func TestRenderSkipsPluginsWithoutSlug(t *testing.T) {
	// Defensive: control-plane is supposed to filter these out, but if
	// one slips through Render shouldn't write a junk YAML.
	plugins := []Plugin{
		{PluginID: "ghost", GrafanaSlug: "", PlatformToken: "tok"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := filepath.Join(cfg.ProvisioningDir, "plugins", "ghost.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected ghost.yaml not to exist; stat err = %v", err)
	}
}

func TestRenderPrunesStalePluginYAML(t *testing.T) {
	// A YAML left over from a now-uninstalled plugin makes Grafana fail
	// provisioning on boot — Render must remove YAMLs not in the desired set.
	cfg := validConfig(t, "")
	outDir := filepath.Join(cfg.ProvisioningDir, "plugins")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := filepath.Join(outDir, "yfinance-app.yaml")
	if err := os.WriteFile(stale, []byte("apiVersion: 1\napps: []\n"), 0o600); err != nil {
		t.Fatalf("seed stale yaml: %v", err)
	}

	plugins := []Plugin{
		{PluginID: "core-app", GrafanaSlug: "core-app", Version: "0.1.0", Type: "app"},
	}
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale yaml not pruned (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "core-app.yaml")); err != nil {
		t.Errorf("desired yaml missing: %v", err)
	}
}

func TestRenderDatasourcePlatformPluginFullConfig(t *testing.T) {
	// type=datasource + platform_plugin=true (core-datasource): the FULL platform
	// datasource render — gateway URLs, platform token, identity jsonData.
	plugins := []Plugin{
		{PluginID: "core-datasource", GrafanaSlug: "core-datasource",
			PlatformToken: "qs-tok", PlatformPlugin: true, Type: "datasource"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	// No app YAML.
	if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "plugins", "core-datasource.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected no app YAML for platform datasource; stat err = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.ProvisioningDir, "datasources", "core-datasource.yaml"))
	if err != nil {
		t.Fatalf("read datasource yaml: %v", err)
	}
	var doc datasourceDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Datasources) != 1 {
		t.Fatalf("len(datasources) = %d, want 1", len(doc.Datasources))
	}
	ds := doc.Datasources[0]
	if ds.Name != "core-datasource" || ds.UID != "core-datasource" {
		t.Errorf("name/uid = %q/%q, want core-datasource", ds.Name, ds.UID)
	}
	if ds.Type != "core-datasource" {
		t.Errorf("type = %q", ds.Type)
	}
	if ds.Access != "proxy" || !ds.Editable {
		t.Errorf("access=%q editable=%v", ds.Access, ds.Editable)
	}
	// Full platform wiring present.
	if ds.JSONData["computeUrl"] != cfg.PluginComputeURL {
		t.Errorf("computeUrl = %v, want %s", ds.JSONData["computeUrl"], cfg.PluginComputeURL)
	}
	// pluginsInstallDir is the plugins symlink dir; the datasource resolves
	// metric refs (`pluginID/metric` -> <dir>/<pluginID>/library-panels/<metric>.py)
	// from it. Must be the install dir, not the SQLite state root (pluginsRoot).
	if ds.JSONData["pluginsInstallDir"] != cfg.PluginsDir {
		t.Errorf("pluginsInstallDir = %v, want %s", ds.JSONData["pluginsInstallDir"], cfg.PluginsDir)
	}
	if ds.JSONData["orgId"] != cfg.OrgID {
		t.Errorf("orgId = %v", ds.JSONData["orgId"])
	}
	if ds.SecureJSONData["platformToken"] != "qs-tok" {
		t.Errorf("platformToken = %v, want qs-tok", ds.SecureJSONData["platformToken"])
	}
}

func TestRenderMinimalDatasourceForCommunityPlugin(t *testing.T) {
	// type=datasource + platform_plugin=false: a bare editable datasource with
	// NO jsonData/secureJsonData.
	plugins := []Plugin{
		{PluginID: "community_ds", GrafanaSlug: "grafana-clickhouse-datasource",
			PlatformToken: "ignored", Type: "datasource"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.ProvisioningDir, "datasources", "community_ds.yaml"))
	if err != nil {
		t.Fatalf("read datasource yaml: %v", err)
	}
	var doc datasourceDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Datasources) != 1 {
		t.Fatalf("len(datasources) = %d, want 1", len(doc.Datasources))
	}
	ds := doc.Datasources[0]
	if ds.Name != "community_ds" || ds.UID != "community_ds" {
		t.Errorf("name/uid = %q/%q, want community_ds", ds.Name, ds.UID)
	}
	if ds.Type != "grafana-clickhouse-datasource" {
		t.Errorf("type = %q, want the grafana slug", ds.Type)
	}
	if ds.Access != "proxy" {
		t.Errorf("access = %q, want proxy", ds.Access)
	}
	if ds.IsDefault {
		t.Errorf("isDefault = true, want false")
	}
	if !ds.Editable {
		t.Errorf("editable = false, want true")
	}
	// No jsonData / secureJsonData — we don't specially configure it.
	if len(ds.JSONData) != 0 {
		t.Errorf("jsonData = %v, want empty", ds.JSONData)
	}
	if len(ds.SecureJSONData) != 0 {
		t.Errorf("secureJsonData = %v, want empty", ds.SecureJSONData)
	}
	// No app YAML either.
	if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "plugins", "community_ds.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected no app YAML; stat err = %v", err)
	}
}

func TestRenderAppPlugin(t *testing.T) {
	// type=app: the app provisioning YAML in plugins/<id>.yaml.
	plugins := []Plugin{
		{PluginID: "core-app", GrafanaSlug: "core-app",
			PlatformToken: "pa-tok", Type: "app"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.ProvisioningDir, "plugins", "core-app.yaml"))
	if err != nil {
		t.Fatalf("read app yaml: %v", err)
	}
	var doc provisioningDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Apps) != 1 {
		t.Fatalf("len(apps) = %d, want 1", len(doc.Apps))
	}
	if doc.Apps[0].Type != "core-app" {
		t.Errorf("type = %q", doc.Apps[0].Type)
	}
	if doc.Apps[0].SecureJSONData["platformToken"] != "pa-tok" {
		t.Errorf("platformToken = %v", doc.Apps[0].SecureJSONData["platformToken"])
	}
	// No datasource YAML.
	if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "datasources", "core-app.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected no datasource YAML for app; stat err = %v", err)
	}
}

func TestRenderPanelProducesNoFile(t *testing.T) {
	// type=panel: panels only need to load; no provisioning entry.
	plugins := []Plugin{
		{PluginID: "fancy_panel", GrafanaSlug: "grafana-clock-panel", Type: "panel"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "plugins", "fancy_panel.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected no app YAML for panel; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "datasources", "fancy_panel.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected no datasource YAML for panel; stat err = %v", err)
	}
}

func TestRenderPlugindataFallsThrough(t *testing.T) {
	// type=datasource + platform_plugin=true + plugin_id=plugindata: the
	// plugindata special-case is gone; it now falls through to renderDatasource
	// and gets the full platform shape (computeUrl + platformToken + pluginTokens).
	plugins := []Plugin{
		{PluginID: "plugindata", GrafanaSlug: "portfoliomanagement-plugindata-datasource",
			PlatformToken: "pd-tok", PlatformPlugin: true, Type: "datasource"},
		{PluginID: "yfinance-app", GrafanaSlug: "yfinance-app",
			PlatformToken: "yf-tok", Type: "app"},
	}
	cfg := validConfig(t, "")
	cfg.PluginComputeURL = "http://127.0.0.1:8790"
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.ProvisioningDir, "datasources", "plugindata.yaml"))
	if err != nil {
		t.Fatalf("read plugindata yaml: %v", err)
	}
	var doc datasourceDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Datasources) != 1 {
		t.Fatalf("len(datasources) = %d, want 1", len(doc.Datasources))
	}
	ds := doc.Datasources[0]
	if ds.JSONData["computeUrl"] != "http://127.0.0.1:8790" {
		t.Errorf("computeUrl = %v, want the sidecar URL", ds.JSONData["computeUrl"])
	}
	if ds.SecureJSONData["platformToken"] != "pd-tok" {
		t.Errorf("platformToken = %v, want pd-tok", ds.SecureJSONData["platformToken"])
	}
	// pluginTokens is present and includes both plugins.
	tokensJSON, ok := ds.SecureJSONData["pluginTokens"]
	if !ok {
		t.Fatal("secureJsonData.pluginTokens not present")
	}
	var tokens map[string]string
	if err := json.Unmarshal([]byte(tokensJSON), &tokens); err != nil {
		t.Fatalf("decode pluginTokens: %v", err)
	}
	if tokens["yfinance-app"] != "yf-tok" {
		t.Errorf("pluginTokens[yfinance-app] = %q, want yf-tok", tokens["yfinance-app"])
	}
}

func TestRenderUnknownTypeNoFileNoPanic(t *testing.T) {
	// Defensive: a plugin whose footprint predates the type field, or an
	// unknown kind. Skip provisioning, do not crash.
	plugins := []Plugin{
		{PluginID: "legacy", GrafanaSlug: "some-grafana-slug", PlatformToken: "tok", Type: ""},
		{PluginID: "weird", GrafanaSlug: "another-slug", Type: "secret-undeclared-kind"},
	}
	cfg := validConfig(t, "")
	var progress bytes.Buffer
	cfg.ProgressW = &progress
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, id := range []string{"legacy", "weird"} {
		if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "plugins", id+".yaml")); !os.IsNotExist(err) {
			t.Errorf("expected no app YAML for %s; stat err = %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "datasources", id+".yaml")); !os.IsNotExist(err) {
			t.Errorf("expected no datasource YAML for %s; stat err = %v", id, err)
		}
	}
	// The skip must surface on the progress stream (the desktop shell consumes
	// it as NDJSON), not be written to a raw stderr the shell never sees.
	want := map[string]progressEvent{
		"legacy": {Event: "plugin", Plugin: "legacy", Status: "skipped", Detail: `unknown grafana type ""`},
		"weird":  {Event: "plugin", Plugin: "weird", Status: "skipped", Detail: `unknown grafana type "secret-undeclared-kind"`},
	}
	got := map[string]progressEvent{}
	for _, line := range strings.Split(strings.TrimSpace(progress.String()), "\n") {
		if line == "" {
			continue
		}
		var ev progressEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode progress line %q: %v", line, err)
		}
		got[ev.Plugin] = ev
	}
	for id, wantEv := range want {
		if gotEv := got[id]; gotEv != wantEv {
			t.Errorf("progress event for %s = %+v, want %+v", id, gotEv, wantEv)
		}
	}
}

func TestWriteUnsignedPluginsList(t *testing.T) {
	// Mixed types, one plugin with empty GrafanaSlug, and a duplicate slug.
	// Only unique non-empty slugs should appear, sorted, comma-joined.
	plugins := []Plugin{
		{PluginID: "core-app", GrafanaSlug: "pm-core-app", Type: "app"},
		{PluginID: "yfinance-app", GrafanaSlug: "pm-yfinance-app-app", Type: "app"},
		{PluginID: "core-datasource", GrafanaSlug: "pm-core-datasource-datasource", PlatformPlugin: true, Type: "datasource"},
		{PluginID: "fancy_panel", GrafanaSlug: "grafana-clock-panel", Type: "panel"},
		{PluginID: "ghost", GrafanaSlug: "", Type: "app"},                          // empty slug — must be excluded
		{PluginID: "dup_alias", GrafanaSlug: "pm-core-app", Type: "app"}, // duplicate slug — must be deduplicated
	}
	cfg := validConfig(t, "")
	if err := writeUnsignedPluginsList(cfg, plugins); err != nil {
		t.Fatalf("writeUnsignedPluginsList: %v", err)
	}
	path := filepath.Join(cfg.ProvisioningDir, "unsigned-plugins")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unsigned-plugins: %v", err)
	}
	got := string(data)
	want := "grafana-clock-panel,pm-core-app,pm-core-datasource-datasource,pm-yfinance-app-app"
	if got != want {
		t.Errorf("unsigned-plugins = %q, want %q", got, want)
	}
	// File must exist at 0o600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat unsigned-plugins: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0o600", info.Mode().Perm())
	}
}

func TestWriteUnsignedPluginsListEmpty(t *testing.T) {
	// No plugins (or all with empty slugs) → empty file must exist (zero bytes).
	plugins := []Plugin{
		{PluginID: "ghost", GrafanaSlug: "", Type: "app"},
	}
	cfg := validConfig(t, "")
	if err := writeUnsignedPluginsList(cfg, plugins); err != nil {
		t.Fatalf("writeUnsignedPluginsList: %v", err)
	}
	path := filepath.Join(cfg.ProvisioningDir, "unsigned-plugins")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read unsigned-plugins: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("unsigned-plugins = %q, want empty file (zero bytes)", string(data))
	}
}

func TestResolveTarget_PinWinsElseLatestValidated(t *testing.T) {
	versions := []VersionStatus{
		{Version: "v1.0.3", Validated: false},
		{Version: "v1.0.2", Validated: true},
		{Version: "v1.0.1", Validated: true},
	}
	if got := resolveTarget("yfinance", map[string]string{"yfinance": "v1.0.3"}, versions); got != "v1.0.3" {
		t.Fatalf("pin: got %q", got)
	}
	if got := resolveTarget("yfinance", nil, versions); got != "v1.0.2" {
		t.Fatalf("latest validated: got %q", got)
	}
	if got := resolveTarget("yfinance", map[string]string{"other": "v9"}, versions); got != "v1.0.2" {
		t.Fatalf("unrelated pin: got %q", got)
	}
	if got := resolveTarget("yfinance", map[string]string{"yfinance": ""}, versions); got != "v1.0.2" {
		t.Fatalf("empty pin treated as no pin: got %q", got)
	}
	// no validated version, no pin -> empty string (caller skips)
	onlyPreview := []VersionStatus{{Version: "v1.0.9", Validated: false}}
	if got := resolveTarget("yfinance", nil, onlyPreview); got != "" {
		t.Fatalf("no validated, no pin: got %q", got)
	}
}

func TestRunEndToEnd(t *testing.T) {
	tgz, sum := makeTarGz(t, map[string]string{
		"gpx_core-app_v2_linux_amd64": "ELF-ish-binary",
		"plugin.json":                        `{"id":"slug-pa"}`,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/art/pa.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tgz)
	})
	var base string
	mux.HandleFunc("/v1/internal/orgs/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]Plugin{{
			PluginID: "core-app", GrafanaSlug: "slug-pa", PlatformToken: "tok-pa",
			Required: true, Type: "app",
		}})
	})
	mux.HandleFunc("/v1/internal/plugins/core-app/versions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plugin_id": "core-app",
			"versions":  []VersionStatus{{Version: "1.0.0", Validated: true}},
		})
	})
	mux.HandleFunc("/v1/internal/plugins/core-app/versions/1.0.0/artifact", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Artifact{
			DownloadURL: base + "/art/pa.tar.gz",
			Sha256:      sum,
			SizeBytes:   int64(len(tgz)),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	cfg := validConfig(t, srv.URL)
	n, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 1 {
		t.Errorf("provisioned %d, want 1", n)
	}
	// YAML rendered.
	if _, err := os.Stat(filepath.Join(cfg.ProvisioningDir, "plugins", "core-app.yaml")); err != nil {
		t.Errorf("yaml not written: %v", err)
	}
	// Binary extracted into the cache + symlinked into the plugins dir.
	link := filepath.Join(cfg.PluginsDir, "slug-pa")
	if _, err := os.Stat(filepath.Join(link, "gpx_core-app_v2_linux_amd64")); err != nil {
		t.Errorf("binary not reachable through plugin symlink: %v", err)
	}
}

func TestFetch_ResolvesVersionLocallyAndFetchesArtifact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/plugins"):
			_, _ = io.WriteString(w, `[{"plugin_id":"yfinance","grafana_slug":"yf","platform_token":"t","type":"app"}]`)
		case strings.HasSuffix(r.URL.Path, "/plugins/yfinance/versions"):
			_, _ = io.WriteString(w, `{"plugin_id":"yfinance","versions":[{"version":"v1.0.3","validated":false},{"version":"v1.0.2","validated":true}]}`)
		case strings.Contains(r.URL.Path, "/versions/v1.0.2/artifact"):
			_, _ = io.WriteString(w, `{"download_url":"http://x/blob","sha256":"abc","size_bytes":1}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := validConfig(t, srv.URL)
	plugins, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Version != "v1.0.2" {
		t.Fatalf("resolved %+v", plugins)
	}
	if plugins[0].Artifact == nil || plugins[0].Artifact.DownloadURL != "http://x/blob" {
		t.Fatalf("artifact %+v", plugins[0].Artifact)
	}
}

func TestParsePins(t *testing.T) {
	if ParsePins("") != nil {
		t.Fatal("empty -> nil")
	}
	if ParsePins("{bad json") != nil {
		t.Fatal("malformed -> nil")
	}
	m := ParsePins(`{"yfinance":"v1.0.3","admin":"v2"}`)
	if m["yfinance"] != "v1.0.3" || m["admin"] != "v2" {
		t.Fatalf("parsed: %+v", m)
	}
}

func TestRenderDatasourcePluginTokensMap(t *testing.T) {
	// core-datasource datasource must carry secureJsonData.pluginTokens = JSON map
	// of every installed plugin's platformToken so it can open foreign SQLites.
	plugins := []Plugin{
		{PluginID: "core-datasource", GrafanaSlug: "core-datasource",
			PlatformToken: "qs-tok", PlatformPlugin: true, Type: "datasource"},
		{PluginID: "yfinance-app", GrafanaSlug: "yfinance-app",
			PlatformToken: "yf-tok", Type: "app"},
		{PluginID: "no_token_plugin", GrafanaSlug: "some-panel", PlatformToken: "", Type: "panel"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.ProvisioningDir, "datasources", "core-datasource.yaml"))
	if err != nil {
		t.Fatalf("read datasource yaml: %v", err)
	}
	var doc datasourceDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ds := doc.Datasources[0]

	// pluginTokens must be present and decode to the expected map.
	tokensJSON, ok := ds.SecureJSONData["pluginTokens"]
	if !ok {
		t.Fatal("secureJsonData.pluginTokens not present")
	}
	var tokens map[string]string
	if err := json.Unmarshal([]byte(tokensJSON), &tokens); err != nil {
		t.Fatalf("decode pluginTokens JSON: %v", err)
	}
	if tokens["yfinance-app"] != "yf-tok" {
		t.Errorf("pluginTokens[yfinance-app] = %q, want yf-tok", tokens["yfinance-app"])
	}
	if tokens["core-datasource"] != "qs-tok" {
		t.Errorf("pluginTokens[core-datasource] = %q, want qs-tok", tokens["core-datasource"])
	}
	// Plugins with empty PlatformToken must be excluded.
	if _, present := tokens["no_token_plugin"]; present {
		t.Errorf("pluginTokens should not include plugin with empty token")
	}
	// Core identity fields still present.
	if ds.JSONData["pluginsRoot"] != cfg.PluginStateDir {
		t.Errorf("pluginsRoot = %v, want %s", ds.JSONData["pluginsRoot"], cfg.PluginStateDir)
	}
	if ds.SecureJSONData["platformToken"] != "qs-tok" {
		t.Errorf("platformToken = %v, want qs-tok", ds.SecureJSONData["platformToken"])
	}
}

func TestPlugindataDatasourceNotProduced(t *testing.T) {
	// After collapsing plugindata into the core-datasource datasource, rendering
	// a plugin set that includes a plugindata plugin must NOT produce a
	// datasources/plugindata.yaml file — the special-case is gone.
	plugins := []Plugin{
		{PluginID: "core-datasource", GrafanaSlug: "core-datasource",
			PlatformToken: "qs-tok", PlatformPlugin: true, Type: "datasource"},
		{PluginID: "yfinance-app", GrafanaSlug: "yfinance-app",
			PlatformToken: "yf-tok", Type: "app"},
	}
	cfg := validConfig(t, "")
	if err := Render(cfg, plugins); err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := filepath.Join(cfg.ProvisioningDir, "datasources", "plugindata.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("plugindata.yaml should not exist; stat err = %v", err)
	}
}

func TestFetch_PinOverridesValidated(t *testing.T) {
	var artifactPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/plugins"):
			_, _ = io.WriteString(w, `[{"plugin_id":"yfinance","grafana_slug":"yf","platform_token":"t","type":"app"}]`)
		case strings.HasSuffix(r.URL.Path, "/plugins/yfinance/versions"):
			_, _ = io.WriteString(w, `{"plugin_id":"yfinance","versions":[{"version":"v1.0.3","validated":false},{"version":"v1.0.2","validated":true}]}`)
		case strings.Contains(r.URL.Path, "/versions/"):
			artifactPath = r.URL.Path
			_, _ = io.WriteString(w, `{"download_url":"http://x/preview","sha256":"xyz","size_bytes":2}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := validConfig(t, srv.URL)
	cfg.Pins = map[string]string{"yfinance": "v1.0.3"}
	plugins, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Version != "v1.0.3" {
		t.Fatalf("pinned version: got %+v", plugins)
	}
	if !strings.Contains(artifactPath, "/versions/v1.0.3/artifact") {
		t.Errorf("artifact path = %q, want .../versions/v1.0.3/artifact...", artifactPath)
	}
}
