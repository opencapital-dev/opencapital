package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/portfolio-management/control-plane/internal/config"
	"github.com/portfolio-management/control-plane/internal/install"
	"github.com/portfolio-management/control-plane/internal/registry"
)

// stubProvider is a registry.PluginProvider that yields no plugins, so the
// catalog Client resolves every id to not-found. Auth-guard tests use it to get
// a non-401 (503/404) once the guard passes without reaching a real registry.
type stubProvider struct{}

func (stubProvider) Plugins(context.Context) ([]*registry.PluginRef, error) { return nil, nil }

// TestInstanceListVersions_AuthGuard calls handleInstanceListVersions directly
// (no HTTP server needed). It verifies:
//  - no bearer → 401
//  - wrong bearer → 401
//  - correct bearer → not 401 (may be 503 because the registry is a stub)
func TestInstanceListVersions_AuthGuard(t *testing.T) {
	const token = "test-bootstrap-token"

	// NewCatalog over a no-op provider returns a non-nil *Client. The handler
	// reaches the registry only after the auth guard passes, so 401-path tests
	// don't need it at all; the "correct bearer" sub-test gets a 503 because the
	// provider yields no plugins (the id resolves to not-found / unreachable).
	reg := registry.NewCatalog(stubProvider{}, nil)

	s := &Server{
		cfg:    config.Config{AdminBootstrapToken: token},
		logger: slog.Default(),
		registry: reg,
	}

	call := func(bearer string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/internal/plugins/yfinance/versions", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		s.handleInstanceListVersions(rr, r)
		return rr.Code
	}

	if got := call(""); got != http.StatusUnauthorized {
		t.Fatalf("no bearer: got %d, want 401", got)
	}
	if got := call("wrong-token"); got != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: got %d, want 401", got)
	}
	if got := call(token); got == http.StatusUnauthorized {
		t.Fatalf("correct bearer: got 401, want non-401 (got %d)", got)
	}
}

// TestInstanceListVersions_ResponseShape mirrors TestListPluginVersions_ReturnsStatus
// (v1_test.go): construct a listPluginVersionsResponse with mixed status entries
// and assert the JSON shape {plugin_id, versions:[{version, validated}]}.
func TestInstanceListVersions_ResponseShape(t *testing.T) {
	resp := listPluginVersionsResponse{
		PluginID: "yfinance",
		Versions: []registry.VersionStatus{
			{Version: "v1.1.0", Validated: false},
			{Version: "v1.0.5", Validated: true},
			{Version: "v1.0.0", Validated: true},
		},
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		PluginID string `json:"plugin_id"`
		Versions []struct {
			Version   string `json:"version"`
			Validated bool   `json:"validated"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PluginID != "yfinance" {
		t.Fatalf("plugin_id = %q, want yfinance", got.PluginID)
	}
	if len(got.Versions) != 3 {
		t.Fatalf("len(versions) = %d, want 3", len(got.Versions))
	}
	// preview (validated=false) first
	if got.Versions[0].Version != "v1.1.0" || got.Versions[0].Validated {
		t.Fatalf("versions[0] = %+v, want {v1.1.0 false}", got.Versions[0])
	}
	// two validated entries
	if got.Versions[1].Version != "v1.0.5" || !got.Versions[1].Validated {
		t.Fatalf("versions[1] = %+v, want {v1.0.5 true}", got.Versions[1])
	}
	if got.Versions[2].Version != "v1.0.0" || !got.Versions[2].Validated {
		t.Fatalf("versions[2] = %+v, want {v1.0.0 true}", got.Versions[2])
	}
}

// TestInstanceArtifact_AuthGuard mirrors TestInstanceListVersions_AuthGuard:
// minimal Server with bootstrap token + port-0 registry stub. Asserts:
//   - no bearer → 401
//   - wrong bearer → 401
//   - correct bearer + missing ?platform → 400
//   - correct bearer + ?platform=darwin-arm64 → neither 401 nor 400 (503 from stub)
func TestInstanceArtifact_AuthGuard(t *testing.T) {
	const token = "test-bootstrap-token"
	reg := registry.NewCatalog(stubProvider{}, nil)
	s := &Server{
		cfg:      config.Config{AdminBootstrapToken: token},
		logger:   slog.Default(),
		registry: reg,
	}

	call := func(bearer, platform string) int {
		url := "/v1/internal/plugins/yfinance/versions/v1.0.0/artifact"
		if platform != "" {
			url += "?platform=" + platform
		}
		r := httptest.NewRequest(http.MethodGet, url, nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		r.SetPathValue("id", "yfinance")
		r.SetPathValue("version", "v1.0.0")
		rr := httptest.NewRecorder()
		s.handleInstanceArtifact(rr, r)
		return rr.Code
	}

	if got := call("", ""); got != http.StatusUnauthorized {
		t.Fatalf("no bearer: got %d, want 401", got)
	}
	if got := call("wrong-token", ""); got != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: got %d, want 401", got)
	}
	if got := call(token, ""); got != http.StatusBadRequest {
		t.Fatalf("correct bearer + missing platform: got %d, want 400", got)
	}
	got := call(token, "darwin-arm64")
	if got == http.StatusUnauthorized || got == http.StatusBadRequest {
		t.Fatalf("correct bearer + platform: got %d, want non-401/non-400", got)
	}
}

// TestInstanceArtifact_ResponseShape asserts the instanceArtifact JSON shape
// uses the expected keys (download_url, sha256, size_bytes).
func TestInstanceArtifact_ResponseShape(t *testing.T) {
	art := instanceArtifact{
		DownloadURL: "https://registry.example.com/v2/plugins/yfinance/blobs/sha256:abc123",
		Sha256:      "abc123def456",
		SizeBytes:   1048576,
	}
	b, err := json.Marshal(art)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["download_url"] != art.DownloadURL {
		t.Fatalf("download_url = %v, want %q", got["download_url"], art.DownloadURL)
	}
	if got["sha256"] != art.Sha256 {
		t.Fatalf("sha256 = %v, want %q", got["sha256"], art.Sha256)
	}
	if got["size_bytes"] != float64(art.SizeBytes) {
		t.Fatalf("size_bytes = %v, want %d", got["size_bytes"], art.SizeBytes)
	}
}

// TestInstancePluginEntryTypeRoundTrips guards the Task B1 plumbing: the
// footprint's Grafana plugin kind (app/datasource/panel) must travel from the
// resolved registry plugin (`rp`, which embeds install.Footprint) into the
// instancePluginEntry the /v1/internal/orgs/{org_id}/plugins handler serves,
// and must serialize under the `type` JSON key the instance-bootstrap Plugin
// struct unmarshals.
//
// The handler resolves `rp` from a live OCI registry + Postgres (no interface
// seams), so this exercises the field copy + JSON shape directly rather than
// the HTTP round-trip: it builds `rp` and `entry` the same way
// handleInstanceListPlugins does, then asserts `type` survives marshaling.
func TestInstancePluginEntryTypeRoundTrips(t *testing.T) {
	// Mirror the handler's resolution: rp embeds the footprint, so rp.Type is
	// the footprint's Type. A1 added Type to install.Footprint.
	rp := registry.Plugin{
		Footprint: install.Footprint{
			PluginID:    "core-datasource",
			GrafanaSlug: "portfolio-core-datasource-datasource",
			Type:        "datasource",
		},
		Required: true,
	}

	// Construct the entry exactly as handleInstanceListPlugins does for the
	// fields under test.
	entry := instancePluginEntry{
		PluginID:       "core-datasource",
		GrafanaSlug:    rp.GrafanaSlug,
		PlatformToken:  "tok",
		Required:       rp.Required,
		Type:           rp.Type,
		PlatformPlugin: rp.PlatformPlugin,
		Version:        "v1.2.3",
	}

	if entry.Type != "datasource" {
		t.Fatalf("entry.Type = %q, want %q (rp.Type must copy the embedded footprint Type)", entry.Type, "datasource")
	}

	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if got["type"] != "datasource" {
		t.Fatalf("response JSON `type` = %v, want %q (field must serialize for instance-bootstrap)", got["type"], "datasource")
	}
}
