//go:build live

package sources

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"os"

	"github.com/portfolio-management/control-plane/internal/manifest"
	"github.com/portfolio-management/control-plane/internal/registry"
)

// Branch under test. The per-plugin manifests live on this branch (plugins/ is
// new here); plugins.json's pointer URLs reference main, so this exercises the
// per-plugin fetch + catalog resolution directly rather than chaining the list
// through main (that chain works once merged).
const (
	branchBase = "https://raw.githubusercontent.com/opencapital-dev/opencapital/feat/federated-plugin-sources"
	listURL    = branchBase + "/plugins.json"
	yfURL      = branchBase + "/plugins/yfinance-app.json"
)

type directProvider struct{ refs []*registry.PluginRef }

func (d directProvider) Plugins(context.Context) ([]*registry.PluginRef, error) { return d.refs, nil }

// TestFederatedE2E_ProductionTag validates the full federated path against REAL
// infrastructure (the branch manifest over HTTPS + the production GHCR trusted
// namespace): bumping yfinance-app to 0.1.3 in its manifest makes 0.1.3 resolve
// as a validated version with a real, sha256-addressed, per-platform artifact,
// pulled anonymously through the new catalog code.
//
//	FED_E2E=1 go test -tags live -run TestFederatedE2E ./internal/sources/ -v
func TestFederatedE2E_ProductionTag(t *testing.T) {
	if os.Getenv("FED_E2E") != "1" {
		t.Skip("set FED_E2E=1 to run the live federated e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. Marketplace LIST parses to curated pointer URLs.
	urls, err := manifest.NewListClient(listURL, nil, time.Minute, nil).Fetch(ctx)
	if err != nil {
		t.Fatalf("fetch marketplace list: %v", err)
	}
	if len(urls) == 0 {
		t.Fatal("marketplace list empty")
	}
	t.Logf("marketplace list: %d pointer(s)", len(urls))

	// 2. Per-plugin manifest parses + carries the new production tag 0.1.3.
	m, err := manifest.NewPluginClient(yfURL, nil, time.Minute, nil).Fetch(ctx)
	if err != nil {
		t.Fatalf("fetch yfinance-app manifest: %v", err)
	}
	if m.PluginID != "yfinance-app" {
		t.Fatalf("pluginId = %q", m.PluginID)
	}
	if !contains(m.Versions, "0.1.3") {
		t.Fatalf("manifest versions %v missing 0.1.3", m.Versions)
	}
	t.Logf("manifest: %s versions=%v", m.PluginID, m.Versions)

	// 3. Build the catalog over this plugin (verified = it is in the official list).
	ref := &registry.PluginRef{
		ManifestURL: yfURL, PluginID: m.PluginID, Publisher: m.Publisher, Verified: true,
		Reg: &registry.Registry{
			Host: m.Registry.Host, Namespace: m.Registry.Namespace,
			StagingNamespace: m.Registry.StagingNamespace, PublicURL: m.Registry.PublicURL,
		},
		Validated: m.Versions,
		Preview:   m.Preview,
	}
	cat := registry.NewCatalog(directProvider{refs: []*registry.PluginRef{ref}}, registry.DefaultRequired)

	// 4. VersionsWithStatus: 0.1.3 present + validated.
	vs, err := cat.VersionsWithStatus(ctx, "yfinance-app")
	if err != nil {
		t.Fatalf("VersionsWithStatus: %v", err)
	}
	var found bool
	for _, v := range vs {
		if normEq(v.Version, "0.1.3") {
			if !v.Validated {
				t.Fatalf("0.1.3 should be validated, got %+v", v)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("0.1.3 not in VersionsWithStatus: %+v", vs)
	}

	// 5. Get: latest validated resolves to 0.1.3 with a real footprint + platforms,
	//    pulled ANONYMOUSLY from production GHCR.
	p, ok, err := cat.Get(ctx, "yfinance-app")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if !normEq(p.Version, "0.1.3") {
		t.Fatalf("Get latest version = %q, want 0.1.3", p.Version)
	}
	if p.PluginID != "yfinance-app" {
		t.Fatalf("footprint pluginId = %q", p.PluginID)
	}
	if len(p.Platforms) == 0 {
		t.Fatal("no platforms on resolved plugin")
	}
	if !p.Source.Verified {
		t.Fatal("source should be verified (in official list)")
	}
	t.Logf("Get: version=%s slug=%s platforms=%v", p.Version, p.GrafanaSlug, p.Platforms)

	// 6. ResolveArtifact for a real platform → 64-hex sha256 + a ghcr.io blob URL.
	plat := pickPlatform(p.Platforms)
	art, ok, err := cat.ResolveArtifact(ctx, "yfinance-app", "0.1.3", plat)
	if err != nil || !ok {
		t.Fatalf("ResolveArtifact(%s): ok=%v err=%v", plat, ok, err)
	}
	if len(art.Sha256) != 64 {
		t.Fatalf("sha256 looks wrong: %q", art.Sha256)
	}
	if !strings.Contains(art.DownloadURL, "ghcr.io/v2/opencapital-dev/plugins/yfinance-app/blobs/sha256:") {
		t.Fatalf("unexpected download URL: %s", art.DownloadURL)
	}
	t.Logf("ResolveArtifact(%s): sha256=%s size=%d", plat, art.Sha256, art.SizeBytes)
	t.Log("E2E OK: production tag v0.1.3 resolves through the federated catalog.")
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func normEq(a, b string) bool { return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v") }

func pickPlatform(platforms []string) string {
	host := runtime.GOOS + "-" + runtime.GOARCH
	for _, p := range platforms {
		if p == host {
			return p
		}
	}
	return platforms[0]
}
