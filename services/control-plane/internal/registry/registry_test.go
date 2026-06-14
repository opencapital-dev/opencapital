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

// fakeProvider is a static PluginProvider for catalog tests.
type fakeProvider struct{ refs []*PluginRef }

func (f fakeProvider) Plugins(context.Context) ([]*PluginRef, error) { return f.refs, nil }

// newFakeCatalog builds a *Client whose single ref points at an httptest OCI
// server serving the given trusted + staging tag maps under the "plugins" /
// "plugins-staging" namespaces. validated/preview are the ref's version sets.
func newFakeCatalog(t *testing.T, trusted, staging map[string][]string, sig map[string]bool, refs []*PluginRef, host string) *Client {
	t.Helper()
	f := &fakeOCIServer{trusted: trusted, staging: staging, sig: sig}
	f.referrerDigest = "sha256:" + sha256Digest(f.referrerManifest())
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	bare := strings.TrimPrefix(srv.URL, "http://")
	for _, r := range refs {
		r.Reg.Host = bare
		r.Reg.PlainHTTP = true
		r.Reg.Namespace = "plugins"
		r.Reg.StagingNamespace = "plugins-staging"
	}
	_ = host
	return NewCatalog(fakeProvider{refs: refs}, nil)
}

func ref(id string, validated, preview []string) *PluginRef {
	return &PluginRef{
		ManifestURL: "https://manifest.test/" + id + ".json",
		PluginID:    id, Publisher: "OpenCapital", Verified: true,
		Reg:       &Registry{},
		Validated: validated, Preview: preview,
	}
}

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

func TestListEmptyWhenRegistryAbsent(t *testing.T) {
	// The ref names a validated version, but the OCI server has NO repo for it
	// (404 NameUnknown) → repoAbsent swallows it → the plugin is skipped, the
	// catalog is empty, and List does not error.
	refs := []*PluginRef{ref("x", []string{"1.0.0"}, nil)}
	c := newFakeCatalog(t,
		map[string][]string{}, // no trusted repos
		map[string][]string{}, // no staging repos
		nil, refs, "")
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty (registry repo absent → 404), got %d", len(got))
	}
}

func TestList_LatestValidatedPerPlugin(t *testing.T) {
	// Trusted tags are v-prefixed (real GHCR); refs hold bare semver.
	refs := []*PluginRef{
		ref("core-app", []string{"1.2.0", "1.1.0"}, nil),
		ref("core-datasource", []string{"0.4.1"}, nil),
		ref("yfinance-app", nil, nil), // no validated, no preview => skipped
	}
	c := newFakeCatalog(t,
		map[string][]string{
			"core-app":        {"v1.1.0", "v1.2.0"},
			"core-datasource": {"v0.4.1"},
		},
		map[string][]string{},
		nil, refs, "")

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d plugins, want 2: %+v", len(got), got)
	}
	byID := map[string]Plugin{}
	for _, p := range got {
		byID[p.PluginID] = p
	}
	core, ok := byID["core-app"]
	if !ok {
		t.Fatalf("core-app missing: %+v", got)
	}
	if core.Version != "1.2.0" {
		t.Fatalf("core-app version = %q, want 1.2.0 (latest validated)", core.Version)
	}
	if core.DisplayName != "Display core-app" {
		t.Fatalf("core-app footprint not read: DisplayName=%q", core.DisplayName)
	}
	if core.Source.URL != "https://manifest.test/core-app.json" || !core.Source.Verified {
		t.Fatalf("core-app source not propagated: %+v", core.Source)
	}
	if _, present := byID["yfinance-app"]; present {
		t.Fatal("yfinance-app (no versions) must NOT appear")
	}
}

func TestList_PreviewOnlyPlugin(t *testing.T) {
	// preview-only: no validated version, footprint read from staging at the
	// highest preview version, Version blanked.
	refs := []*PluginRef{
		ref("core-app", []string{"1.2.0", "1.1.0"}, nil),
		ref("preview-only", nil, []string{"0.2.0", "0.1.0"}),
		ref("missing-build", nil, []string{"9.9.9"}),
	}
	c := newFakeCatalog(t,
		map[string][]string{"core-app": {"v1.1.0", "v1.2.0"}},
		map[string][]string{
			"preview-only":  {"v0.1.0", "v0.2.0"},
			"missing-build": {"v0.0.1"},
		},
		nil, refs, "")

	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]Plugin{}
	for _, p := range got {
		byID[p.PluginID] = p
	}
	prev, ok := byID["preview-only"]
	if !ok {
		t.Fatalf("preview-only must appear: %+v", got)
	}
	if prev.Version != "" {
		t.Fatalf("preview-only Version = %q, want empty", prev.Version)
	}
	if prev.DisplayName != "Display preview-only" {
		t.Fatalf("preview-only footprint not read from staging: DisplayName=%q", prev.DisplayName)
	}
	if _, present := byID["missing-build"]; present {
		t.Fatal("missing-build (preview absent from staging) must NOT appear")
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d plugins, want 2 (core-app, preview-only): %+v", len(got), got)
	}
}

func TestVersionsWithStatus_UnionValidatedAndPreview(t *testing.T) {
	c := NewCatalog(fakeProvider{refs: []*PluginRef{
		ref("core-app", []string{"1.2.0", "1.1.0"}, []string{"1.3.0", "1.2.0"}),
	}}, nil)
	got, err := c.VersionsWithStatus(context.Background(), "core-app")
	if err != nil {
		t.Fatalf("VersionsWithStatus: %v", err)
	}
	want := []VersionStatus{
		{Version: "1.3.0", Validated: false},
		{Version: "1.2.0", Validated: true},
		{Version: "1.1.0", Validated: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestVersionsWithStatus_UnknownID(t *testing.T) {
	c := NewCatalog(fakeProvider{refs: nil}, nil)
	got, err := c.VersionsWithStatus(context.Background(), "nope")
	if err != nil {
		t.Fatalf("VersionsWithStatus: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown id should yield empty, got %+v", got)
	}
}

func TestResolveArtifact_TrustedPath(t *testing.T) {
	refs := []*PluginRef{ref("yfinance", []string{"1.0.2"}, []string{"1.0.3"})}
	c := newFakeCatalog(t,
		map[string][]string{"yfinance": {"v1.0.2"}},
		map[string][]string{"yfinance": {"v1.0.2", "v1.0.3"}},
		nil, refs, "")
	art, ok, err := c.ResolveArtifact(context.Background(), "yfinance", "v1.0.2", "darwin-arm64")
	if err != nil || !ok {
		t.Fatalf("ResolveArtifact trusted: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(art.DownloadURL, "/plugins/") {
		t.Fatalf("expected trusted namespace in URL, got %q", art.DownloadURL)
	}
}

func TestResolveArtifact_FallsBackToStagingForPreview(t *testing.T) {
	refs := []*PluginRef{ref("yfinance", []string{"1.0.2"}, []string{"1.0.3"})}
	c := newFakeCatalog(t,
		map[string][]string{"yfinance": {"v1.0.2"}},
		map[string][]string{"yfinance": {"v1.0.2", "v1.0.3"}},
		nil, refs, "")
	art, ok, err := c.ResolveArtifact(context.Background(), "yfinance", "v1.0.3", "darwin-arm64")
	if err != nil || !ok {
		t.Fatalf("ResolveArtifact preview: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(art.DownloadURL, "/plugins-staging/") {
		t.Fatalf("expected staging namespace in URL, got %q", art.DownloadURL)
	}
}

func TestGetVersion_TrustedThenStaging(t *testing.T) {
	refs := []*PluginRef{ref("yfinance", []string{"1.0.2"}, []string{"1.0.3"})}
	c := newFakeCatalog(t,
		map[string][]string{"yfinance": {"v1.0.2"}},
		map[string][]string{"yfinance": {"v1.0.2", "v1.0.3"}},
		nil, refs, "")
	// trusted tag
	p, found, err := c.GetVersion(context.Background(), "yfinance", "v1.0.2")
	if err != nil || !found {
		t.Fatalf("GetVersion trusted: found=%v err=%v", found, err)
	}
	if p.Version != "v1.0.2" {
		t.Fatalf("version = %q want v1.0.2", p.Version)
	}
	// staging-only tag falls back
	p, found, err = c.GetVersion(context.Background(), "yfinance", "v1.0.3")
	if err != nil || !found {
		t.Fatalf("GetVersion staging fallback: found=%v err=%v", found, err)
	}
	if p.Source.URL == "" {
		t.Fatalf("GetVersion must propagate source")
	}
}

func TestGet_LatestValidated(t *testing.T) {
	refs := []*PluginRef{ref("core-app", []string{"1.2.0", "1.1.0"}, nil)}
	c := newFakeCatalog(t,
		map[string][]string{"core-app": {"v1.1.0", "v1.2.0"}},
		map[string][]string{},
		nil, refs, "")
	p, found, err := c.Get(context.Background(), "core-app")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if p.Version != "1.2.0" {
		t.Fatalf("version = %q want 1.2.0", p.Version)
	}
	if !p.Source.Verified {
		t.Fatalf("Get must propagate source: %+v", p.Source)
	}
}

func TestStagingTagSigned_GHCRMislabeledIndex(t *testing.T) {
	s := newFakeStaging(t,
		map[string][]string{},
		map[string][]string{"core-datasource": {"v1.0.0"}},
		map[string]bool{"plugins-staging/core-datasource": true})
	signed, err := s.StagingTagSigned(context.Background(), "core-datasource", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !signed {
		t.Fatal("StagingTagSigned must report signed despite GHCR's mislabeled index artifactType")
	}
}

func TestStagingTagSigned_Unsigned(t *testing.T) {
	s := newFakeStaging(t,
		map[string][]string{},
		map[string][]string{"core-datasource": {"v1.0.0"}},
		nil)
	signed, err := s.StagingTagSigned(context.Background(), "core-datasource", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if signed {
		t.Fatal("StagingTagSigned must report unsigned when there is no signature referrer")
	}
}

func TestStagingListVersions(t *testing.T) {
	s := newFakeStaging(t,
		map[string][]string{"yfinance": {"v1.0.1", "v1.0.2"}},
		map[string][]string{"yfinance": {"v1.0.1", "v1.0.2", "v1.0.3"}},
		nil)
	trusted, err := s.ListVersions(context.Background(), "yfinance")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if !reflect.DeepEqual(trusted, []string{"v1.0.2", "v1.0.1"}) {
		t.Fatalf("trusted = %v", trusted)
	}
	staged, err := s.ListStagingVersions(context.Background(), "yfinance")
	if err != nil {
		t.Fatalf("ListStagingVersions: %v", err)
	}
	if !reflect.DeepEqual(staged, []string{"v1.0.3", "v1.0.2", "v1.0.1"}) {
		t.Fatalf("staged = %v", staged)
	}
}

func TestNewStagingCanPrune(t *testing.T) {
	if NewStaging("https://ghcr.io", "p", "p-staging", "", "").CanPruneStaging() {
		t.Fatal("no creds, no deleter → cannot prune")
	}
	if !NewStaging("https://ghcr.io", "p", "p-staging", "user", "pat").CanPruneStaging() {
		t.Fatal("basic auth → can prune")
	}
}

// newFakeStaging builds a *StagingClient pointed at an httptest OCI server with
// the given trusted + staging tag maps.
func newFakeStaging(t *testing.T, trusted, staging map[string][]string, sig map[string]bool) *StagingClient {
	t.Helper()
	f := &fakeOCIServer{trusted: trusted, staging: staging, sig: sig}
	f.referrerDigest = "sha256:" + sha256Digest(f.referrerManifest())
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewStaging(srv.URL, "plugins", "plugins-staging", "", "")
}
