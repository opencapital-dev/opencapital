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

// SourceStore reads the user-added per-plugin manifest URLs.
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

// Provider yields the federated catalog's PluginRefs: the official marketplace
// list (verified) ∪ enabled user-added DB rows (unverified), each resolved to a
// parsed per-plugin manifest. Satisfies registry.PluginProvider.
type Provider struct {
	store   SourceStore
	list    ListFetcher
	plugins PluginFetcher
}

// New builds a Provider over a source store, marketplace-list fetcher, and
// per-plugin manifest fetcher.
func New(st SourceStore, list ListFetcher, plugins PluginFetcher) *Provider {
	return &Provider{store: st, list: list, plugins: plugins}
}

// Plugins returns one PluginRef per reachable manifest: the official list URLs
// (Verified=true) followed by enabled user-added URLs not already official
// (Verified=false). A list-fetch failure degrades to "no official set" rather
// than blanking the user-added plugins, and an unreachable individual manifest
// is skipped rather than failing the whole catalog.
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

// sortDesc returns vs sorted greatest-semver first, tolerating the bare-semver
// form by trying a "v" prefix when normalizing.
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
