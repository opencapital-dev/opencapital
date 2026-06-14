package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const emptyConfigType = "application/vnd.oci.empty.v1+json"

// fakeOCIServer starts an httptest server that serves OCI tag-list and manifest
// responses for two namespaces (trusted and staging). Both maps are keyed by plugin id.
type fakeOCIServer struct {
	trusted        map[string][]string // plugin id -> tags
	staging        map[string][]string // plugin id -> tags
	sig            map[string]bool     // "<ns>/<id>" -> has a bundle signature referrer
	referrerDigest string              // "sha256:" + digest of referrerManifest()
}

func (f *fakeOCIServer) referrerManifest() []byte {
	man := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"artifactType":  sigstoreBundleMediaType,
		"config":        map[string]any{"mediaType": emptyConfigType, "digest": "sha256:" + sha256Digest([]byte("empty")), "size": len([]byte("empty"))},
		"layers": []map[string]any{{
			"mediaType": sigstoreBundleMediaType,
			"digest":    "sha256:" + sha256Digest([]byte("bundle-blob")),
			"size":      len([]byte("bundle-blob")),
		}},
	}
	b, _ := json.Marshal(man)
	return b
}

func (f *fakeOCIServer) referrerIndex() []byte {
	rm := f.referrerManifest()
	idx := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType":    "application/vnd.oci.image.manifest.v1+json",
			"artifactType": emptyConfigType, // GHCR mislabel
			"digest":       "sha256:" + sha256Digest(rm),
			"size":         len(rm),
		}},
	}
	b, _ := json.Marshal(idx)
	return b
}

func (f *fakeOCIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	parts := strings.Split(path, "/")

	// /v2/<ns>/<id>/tags/list
	if len(parts) == 4 && parts[2] == "tags" && parts[3] == "list" {
		ns, id := parts[0], parts[1]
		var tags []string
		switch ns {
		case "plugins":
			tags = f.trusted[id]
		case "plugins-staging":
			tags = f.staging[id]
		}
		if tags == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"NAME_UNKNOWN"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": ns + "/" + id, "tags": tags})
		return
	}

	// /v2/<ns>/<id>/referrers/<digest>  -> 404 (force tag-schema fallback, like GHCR)
	if len(parts) == 4 && parts[2] == "referrers" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// fallback referrers tag: /v2/<ns>/<id>/manifests/sha256-<hex> -> the mislabeled index
	if len(parts) == 4 && parts[2] == "manifests" && strings.HasPrefix(parts[3], "sha256-") {
		ns, id := parts[0], parts[1]
		if !f.sig[ns+"/"+id] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := f.referrerIndex()
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Digest(body))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}

	// referrer manifest by digest -> the bundle manifest (correct artifactType + layer)
	if len(parts) == 4 && parts[2] == "manifests" && parts[3] == f.referrerDigest {
		body := f.referrerManifest()
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Digest(body))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}

	// /v2/<ns>/<id>/blobs/<digest> -> the footprint config blob
	if len(parts) == 4 && parts[2] == "blobs" {
		id := parts[1]
		body := fakeFootprintBlob(id)
		if "sha256:"+sha256Digest(body) != parts[3] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.config.v1+json")
		w.Header().Set("Docker-Content-Digest", parts[3])
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}

	// /v2/<ns>/<id>/manifests/<tag>
	if len(parts) == 4 && parts[2] == "manifests" {
		ns, id, tag := parts[0], parts[1], parts[3]
		var tags []string
		switch ns {
		case "plugins":
			tags = f.trusted[id]
		case "plugins-staging":
			tags = f.staging[id]
		}
		found := false
		for _, t := range tags {
			if t == tag {
				found = true
				break
			}
		}
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"NAME_UNKNOWN"}]}`))
			return
		}
		body := fakeManifest(ns, id, tag)
		dgst := sha256Digest(body)
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+dgst)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}

	http.NotFound(w, r)
}

// fakeManifest returns a minimal OCI manifest JSON with one layer annotated for
// "darwin-arm64", so ResolveArtifact can find a platform match.
func fakeManifest(ns, id, tag string) []byte {
	layerDigest := "sha256:" + sha256Digest([]byte(ns+"/"+id+":"+tag))
	cfg := fakeFootprintBlob(id)
	man := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    "sha256:" + sha256Digest(cfg),
			"size":      len(cfg),
		},
		"layers": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    layerDigest,
				"size":      42,
				"annotations": map[string]string{
					platformAnnotation: "darwin-arm64",
				},
			},
		},
	}
	b, _ := json.Marshal(man)
	return b
}

func sha256Digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fakeFootprintBlob returns a deterministic footprint config blob for a plugin
// id, so List can read DisplayName/Type/GrafanaSlug back out of the registry.
func fakeFootprintBlob(id string) []byte {
	fp := map[string]any{
		"plugin_id":    id,
		"grafana_slug": "opencapital-" + id,
		"type":         "app",
		"display_name": "Display " + id,
		"description":  "desc " + id,
	}
	b, _ := json.Marshal(fp)
	return b
}

// fakeManifestSource implements ManifestSource from two in-memory
// plugin->versions maps (bare-semver, any order — the accessors sort
// semver-desc). plugins = validated section, preview = preview section.
type fakeManifestSource struct {
	plugins map[string][]string
	preview map[string][]string
}

func fakeSortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (f fakeManifestSource) PluginIDs(_ context.Context) ([]string, error) {
	return fakeSortedKeys(f.plugins), nil
}

func (f fakeManifestSource) ValidatedVersions(_ context.Context, id string) ([]string, error) {
	return sortSemverDesc(f.plugins[id]), nil
}

func (f fakeManifestSource) PreviewPluginIDs(_ context.Context) ([]string, error) {
	return fakeSortedKeys(f.preview), nil
}

func (f fakeManifestSource) PreviewVersions(_ context.Context, id string) ([]string, error) {
	return sortSemverDesc(f.preview[id]), nil
}

// fakeEnum implements RepoEnumerator backed by the same in-memory maps as fakeOCIServer.
type fakeEnum struct{ trusted, staging map[string][]string }

func (f fakeEnum) ReposWithPrefix(_ context.Context, prefix string) ([]string, error) {
	m := f.trusted
	if prefix == "plugins-staging/" {
		m = f.staging
	}
	var out []string
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// newFakeClient builds a *Client pointed at an httptest server serving the given
// trusted and staging tag maps. sig is keyed by "<ns>/<id>" and marks repos that
// have a bundle signature referrer (emulating GHCR's mislabeled referrers index).
// Pass nil for sig when referrer support is not needed.
func newFakeClient(t *testing.T, trusted, staging map[string][]string, sig map[string]bool) *Client {
	t.Helper()
	f := &fakeOCIServer{trusted: trusted, staging: staging, sig: sig}
	f.referrerDigest = "sha256:" + sha256Digest(f.referrerManifest())
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	// Strip http:// — New parses it back out.
	c := New(srv.URL, srv.URL, "plugins", "plugins-staging", nil, "", "")
	return c.WithEnumerator(fakeEnum{trusted, staging})
}

func TestVersionsWithStatus_ManifestValidatedAndPreview(t *testing.T) {
	// Validated set from .plugins, preview set from .preview; no oras read.
	c := newFakeClient(t,
		map[string][]string{},
		map[string][]string{},
		nil,
	).WithManifest(fakeManifestSource{
		plugins: map[string][]string{"yfinance": {"1.0.1", "1.0.2"}},
		preview: map[string][]string{"yfinance": {"1.0.3"}},
	})
	got, err := c.VersionsWithStatus(context.Background(), "yfinance")
	if err != nil {
		t.Fatalf("VersionsWithStatus: %v", err)
	}
	want := []VersionStatus{
		{Version: "1.0.3", Validated: false},
		{Version: "1.0.2", Validated: true},
		{Version: "1.0.1", Validated: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestVersionsWithStatus_ManifestNoPreview(t *testing.T) {
	c := newFakeClient(t,
		map[string][]string{},
		map[string][]string{},
		nil,
	).WithManifest(fakeManifestSource{
		plugins: map[string][]string{"yfinance": {"1.0.1"}},
		preview: map[string][]string{}, // no preview versions
	})
	got, err := c.VersionsWithStatus(context.Background(), "yfinance")
	if err != nil {
		t.Fatalf("VersionsWithStatus: %v", err)
	}
	want := []VersionStatus{{Version: "1.0.1", Validated: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestVersionsWithStatus_NoManifestFallsBackToTrusted(t *testing.T) {
	// Without a ManifestSource, only the trusted namespace is read (all
	// Validated); the staging namespace is NOT consulted for the version list.
	c := newFakeClient(t,
		map[string][]string{"yfinance": {"v1.0.1", "v1.0.2"}},
		map[string][]string{"yfinance": {"v1.0.1", "v1.0.2", "v1.0.3"}},
		nil,
	)
	got, err := c.VersionsWithStatus(context.Background(), "yfinance")
	if err != nil {
		t.Fatalf("VersionsWithStatus: %v", err)
	}
	want := []VersionStatus{
		{Version: "v1.0.2", Validated: true},
		{Version: "v1.0.1", Validated: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestList_FromManifest_LatestValidatedPerPlugin(t *testing.T) {
	// Trusted tags are v-prefixed (real GHCR); manifest holds bare semver.
	c := newFakeClient(t,
		map[string][]string{
			"core-app":        {"v1.1.0", "v1.2.0"},
			"core-datasource": {"v0.4.1"},
			"yfinance-app":    {"v0.9.0"},
		},
		map[string][]string{},
		nil,
	).WithManifest(fakeManifestSource{plugins: map[string][]string{
		"core-app":        {"1.2.0", "1.1.0"},
		"core-datasource": {"0.4.1"},
		"yfinance-app":    {}, // validated list empty AND no preview => skipped
	}})

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d plugins, want 2 (yfinance-app has no validated version)", len(got))
	}
	byID := map[string]Plugin{}
	for _, p := range got {
		byID[p.PluginID] = p
	}
	core, ok := byID["core-app"]
	if !ok {
		t.Fatalf("core-app missing from catalog: %+v", got)
	}
	if core.Version != "v1.2.0" {
		t.Fatalf("core-app version = %q, want v1.2.0 (latest validated, v-prefixed tag)", core.Version)
	}
	if core.DisplayName != "Display core-app" {
		t.Fatalf("core-app footprint not read: DisplayName=%q", core.DisplayName)
	}
	if _, present := byID["yfinance-app"]; present {
		t.Fatal("yfinance-app (empty validated list) must NOT appear in the catalog")
	}
}

func TestList_FromManifest_IncludesPreviewOnlyPlugins(t *testing.T) {
	// core-app: validated -> appears with its validated Version.
	// preview-only: in the manifest's preview section only (no validated
	//   version) WITH a matching staging build -> appears with empty Version,
	//   footprint read from staging at the HIGHEST preview version.
	// missing-build: named in the preview section but its highest preview
	//   version has NO staging artifact -> omitted (the artifact lookup misses).
	c := newFakeClient(t,
		map[string][]string{
			"core-app": {"v1.1.0", "v1.2.0"},
		},
		map[string][]string{
			// staging tags are v-prefixed; the manifest names bare semver.
			"preview-only": {"v0.1.0", "v0.2.0"},
			// missing-build has a staging repo but NOT the v9.9.9 the manifest names.
			"missing-build": {"v0.0.1"},
		},
		nil,
	).WithManifest(fakeManifestSource{
		plugins: map[string][]string{
			"core-app": {"1.2.0", "1.1.0"},
		},
		preview: map[string][]string{
			"preview-only":  {"0.2.0", "0.1.0"}, // highest = 0.2.0 -> staging v0.2.0
			"missing-build": {"9.9.9"},          // no v9.9.9 in staging => omitted
		},
	})

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]Plugin{}
	for _, p := range got {
		byID[p.PluginID] = p
	}

	// Validated plugin: real validated version surfaced.
	core, ok := byID["core-app"]
	if !ok {
		t.Fatalf("core-app missing from catalog: %+v", got)
	}
	if core.Version != "v1.2.0" {
		t.Fatalf("core-app version = %q, want v1.2.0 (latest validated)", core.Version)
	}

	// Preview-only: appears, Version empty, footprint sourced from the staging
	// artifact at the highest preview version (v0.2.0, via bare->v fallback).
	prev, ok := byID["preview-only"]
	if !ok {
		t.Fatalf("preview-only must appear (has a staging build at its preview version): %+v", got)
	}
	if prev.Version != "" {
		t.Fatalf("preview-only Version = %q, want empty (no validated version)", prev.Version)
	}
	if prev.DisplayName != "Display preview-only" {
		t.Fatalf("preview-only footprint not read from staging: DisplayName=%q", prev.DisplayName)
	}

	// missing-build: preview version named but no staging artifact => omitted.
	if _, present := byID["missing-build"]; present {
		t.Fatal("missing-build (preview version absent from staging) must NOT appear")
	}

	if len(got) != 2 {
		t.Fatalf("List returned %d plugins, want 2 (core-app, preview-only): %+v", len(got), got)
	}
}

func TestList_FromManifest_ValidatedWinsOverPreview(t *testing.T) {
	// A plugin present in BOTH the validated and preview sections appears ONCE,
	// on the validated path (Version set to the validated version).
	c := newFakeClient(t,
		map[string][]string{
			"core-app": {"v1.2.0"},
		},
		map[string][]string{
			"core-app": {"v1.2.0", "v1.3.0"}, // a newer preview build also staged
		},
		nil,
	).WithManifest(fakeManifestSource{
		plugins: map[string][]string{"core-app": {"1.2.0"}},
		preview: map[string][]string{"core-app": {"1.3.0", "1.2.0"}},
	})

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d plugins, want 1 (core-app once): %+v", len(got), got)
	}
	if got[0].Version != "v1.2.0" {
		t.Fatalf("core-app version = %q, want v1.2.0 (validated path wins)", got[0].Version)
	}
}

func TestVersionsWithStatus_FromManifest(t *testing.T) {
	// The version LIST is the union of the manifest's validated and preview
	// sections — NO oras staging-tag listing. preview has 1.2.0 (also validated)
	// and 1.3.0 (preview-only). validated also has 1.1.0 (preview-pruned), which
	// must still appear.
	c := newFakeClient(t,
		map[string][]string{},
		map[string][]string{}, // no staging-OCI read at all
		nil,
	).WithManifest(fakeManifestSource{
		plugins: map[string][]string{"core-app": {"1.2.0", "1.1.0"}},
		preview: map[string][]string{"core-app": {"1.3.0", "1.2.0"}},
	})

	got, err := c.VersionsWithStatus(context.Background(), "core-app")
	if err != nil {
		t.Fatalf("VersionsWithStatus: %v", err)
	}
	want := []VersionStatus{
		{Version: "1.3.0", Validated: false}, // preview-only
		{Version: "1.2.0", Validated: true},  // preview + validated (validated form reported)
		{Version: "1.1.0", Validated: true},  // validated-only
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestResolveArtifact_TrustedPath(t *testing.T) {
	c := newFakeClient(t,
		map[string][]string{"yfinance": {"v1.0.2"}},
		map[string][]string{"yfinance": {"v1.0.2", "v1.0.3"}},
		nil,
	)
	art, ok, err := c.ResolveArtifact(context.Background(), "yfinance", "v1.0.2", "darwin-arm64")
	if err != nil || !ok {
		t.Fatalf("ResolveArtifact trusted: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(art.DownloadURL, "/plugins/") {
		t.Fatalf("expected trusted namespace in URL, got %q", art.DownloadURL)
	}
}

func TestStagingTagSigned_GHCRMislabeledIndex(t *testing.T) {
	c := newFakeClient(t,
		map[string][]string{},
		map[string][]string{"core-datasource": {"v1.0.0"}},
		map[string]bool{"plugins-staging/core-datasource": true},
	)
	signed, err := c.StagingTagSigned(context.Background(), "core-datasource", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !signed {
		t.Fatal("StagingTagSigned must report signed despite GHCR's mislabeled index artifactType")
	}
}

func TestStagingTagSigned_Unsigned(t *testing.T) {
	c := newFakeClient(t,
		map[string][]string{},
		map[string][]string{"core-datasource": {"v1.0.0"}},
		nil, // no signature referrer
	)
	signed, err := c.StagingTagSigned(context.Background(), "core-datasource", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if signed {
		t.Fatal("StagingTagSigned must report unsigned when there is no signature referrer")
	}
}

func TestResolveArtifact_FallsBackToStagingForPreview(t *testing.T) {
	c := newFakeClient(t,
		map[string][]string{"yfinance": {"v1.0.2"}},
		map[string][]string{"yfinance": {"v1.0.2", "v1.0.3"}},
		nil,
	)
	art, ok, err := c.ResolveArtifact(context.Background(), "yfinance", "v1.0.3", "darwin-arm64")
	if err != nil || !ok {
		t.Fatalf("ResolveArtifact preview: ok=%v err=%v", ok, err)
	}
	if art.DownloadURL == "" {
		t.Fatalf("expected a staging download URL")
	}
	if !strings.Contains(art.DownloadURL, "/plugins-staging/") {
		t.Fatalf("expected staging namespace in URL, got %q", art.DownloadURL)
	}
}
