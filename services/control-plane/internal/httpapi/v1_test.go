package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/portfolio-management/control-plane/internal/install"
	"github.com/portfolio-management/control-plane/internal/registry"
)

// TestMarketplaceEntryTypeRoundTrips guards the Task C1 plumbing: the
// footprint's Grafana plugin kind (app/datasource/panel) must travel from the
// resolved registry plugin (`rp`) into the marketplaceEntry the
// GET /v1/marketplace/catalog handler serves, and must serialize under the
// `type` JSON key.
func TestMarketplaceEntryTypeRoundTrips(t *testing.T) {
	rp := registry.Plugin{
		Footprint: install.Footprint{
			PluginID:    "core-datasource",
			GrafanaSlug: "portfolio-core-datasource-datasource",
			Type:        "datasource",
		},
		Required: true,
		Version:  "v1.2.3",
		Source:   registry.SourceInfo{URL: "u", Publisher: "OpenCapital", Verified: true},
	}

	// Construct the entry exactly as handleV1MarketplaceCatalog does.
	entry := marketplaceEntry{
		PluginID:               rp.PluginID,
		GrafanaSlug:            rp.GrafanaSlug,
		DisplayName:            rp.DisplayName,
		Description:            rp.Description,
		Type:                   rp.Type,
		Required:               rp.Required,
		LatestValidatedVersion: rp.Version,
	}
	entry.Source = rp.Source

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
		t.Fatalf("response JSON `type` = %v, want %q (field must serialize for marketplace catalog)", got["type"], "datasource")
	}
	src, ok := got["source"].(map[string]any)
	if !ok {
		t.Fatalf("source field missing or wrong type: %v", got["source"])
	}
	if src["verified"] != true {
		t.Fatalf("source.verified = %v, want true", src["verified"])
	}
}

// TestListPluginVersions_ReturnsStatus guards the B1 plumbing: the
// listPluginVersionsResponse must carry []registry.VersionStatus (not []string)
// and each element must serialize with the "version" + "validated" JSON keys.
func TestListPluginVersions_ReturnsStatus(t *testing.T) {
	resp := listPluginVersionsResponse{
		PluginID: "yfinance",
		Versions: []registry.VersionStatus{
			{Version: "v1.0.3", Validated: false},
			{Version: "v1.0.2", Validated: true},
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
	if len(got.Versions) != 2 {
		t.Fatalf("len(versions) = %d, want 2", len(got.Versions))
	}
	if got.Versions[0].Version != "v1.0.3" || got.Versions[0].Validated != false {
		t.Fatalf("versions[0] = %+v, want {v1.0.3 false}", got.Versions[0])
	}
	if got.Versions[1].Version != "v1.0.2" || got.Versions[1].Validated != true {
		t.Fatalf("versions[1] = %+v, want {v1.0.2 true}", got.Versions[1])
	}
}

// TestMarketplaceEntryShape guards the E5 cleanup: marketplaceEntry must
// serialize with latest_validated_version and must NOT carry
// installed_version, update_available, or latest_version.
func TestMarketplaceEntryShape(t *testing.T) {
	entry := marketplaceEntry{
		PluginID:               "yfinance",
		Type:                   "app",
		Installed:              true,
		InstalledAt:            "2026-01-01T00:00:00Z",
		LatestValidatedVersion: "v1.5.0",
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["latest_validated_version"] != "v1.5.0" {
		t.Fatalf("latest_validated_version = %v, want v1.5.0", got["latest_validated_version"])
	}
	for _, banned := range []string{"installed_version", "update_available", "latest_version"} {
		if _, present := got[banned]; present {
			t.Fatalf("field %q must not appear in marketplaceEntry JSON", banned)
		}
	}
}
