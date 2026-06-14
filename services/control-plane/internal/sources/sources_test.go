package sources

import (
	"context"
	"testing"

	"github.com/portfolio-management/control-plane/internal/manifest"
	"github.com/portfolio-management/control-plane/internal/store"
)

type fakeStore struct{ rows []store.PluginSource }

func (f fakeStore) ListPluginSources(context.Context) ([]store.PluginSource, error) {
	return f.rows, nil
}

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
