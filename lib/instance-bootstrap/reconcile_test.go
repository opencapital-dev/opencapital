package bootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// makeTarGz builds an in-memory gzip tarball from name->content and returns
// the bytes plus their sha256 hex.
func makeTarGz(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

// artifactServer serves a tarball and counts how many times it's fetched.
func artifactServer(t *testing.T, tgz []byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func reconcileCfg(t *testing.T) Config {
	t.Helper()
	return Config{
		OrgID:                 "00000000-0000-0000-0000-000000000001",
		ControlPlaneURL:       "http://unused",
		BootstrapToken:        "tok",
		ProvisioningDir:       t.TempDir(),
		PluginControlPlaneURL: "http://cp",
		PluginGatewayURL:      "http://gw",
		Platform:              "linux-amd64",
		PluginCacheDir:        t.TempDir(),
		PluginsDir:            t.TempDir(),
		ProgressW:             io.Discard,
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	tgz, sum := makeTarGz(t, map[string]string{"gpx_demo_linux_amd64": "bin", "plugin.json": "{}"})
	srv, hits := artifactServer(t, tgz)
	cfg := reconcileCfg(t)
	p := Plugin{
		PluginID: "demo", GrafanaSlug: "demo-app", Required: true, Version: "1.0.0",
		Artifact: &Artifact{DownloadURL: srv.URL, Sha256: sum, SizeBytes: int64(len(tgz))},
	}
	if err := install(context.Background(), cfg, p); err != nil {
		t.Fatalf("install #1: %v", err)
	}
	if err := install(context.Background(), cfg, p); err != nil {
		t.Fatalf("install #2: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("downloaded %d times, want 1 (second run should hit cache)", got)
	}
	// Binary reachable through the symlink.
	if _, err := os.Stat(filepath.Join(cfg.PluginsDir, "demo-app", "gpx_demo_linux_amd64")); err != nil {
		t.Errorf("binary not linked: %v", err)
	}
}

func TestInstallShaMismatchFails(t *testing.T) {
	tgz, _ := makeTarGz(t, map[string]string{"gpx_demo_linux_amd64": "bin"})
	srv, _ := artifactServer(t, tgz)
	cfg := reconcileCfg(t)
	p := Plugin{
		PluginID: "demo", GrafanaSlug: "demo-app", Version: "1.0.0",
		Artifact: &Artifact{DownloadURL: srv.URL, Sha256: "deadbeef", SizeBytes: int64(len(tgz))},
	}
	err := install(context.Background(), cfg, p)
	if err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
	// Nothing should have been published to the cache.
	if _, statErr := os.Stat(filepath.Join(cfg.PluginCacheDir, "demo", "1.0.0", "linux-amd64", markerFile)); !os.IsNotExist(statErr) {
		t.Errorf("cache marker should not exist after a failed verify")
	}
}

func TestUntarRejectsZipSlip(t *testing.T) {
	// Craft a tarball with an entry that escapes the target dir.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "pwned"
	_ = tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()

	tmp := filepath.Join(t.TempDir(), "evil.tar.gz")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := untar(tmp, dest); err == nil {
		t.Fatal("expected zip-slip rejection, got nil")
	}
	// The escaped file must not have been written.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); !os.IsNotExist(err) {
		t.Errorf("zip-slip wrote outside target dir")
	}
}

func TestRequiredNoArtifactAborts(t *testing.T) {
	cfg := reconcileCfg(t)
	_, err := installAll(context.Background(), cfg, []Plugin{
		{PluginID: "demo", GrafanaSlug: "demo-app", Required: true, Version: "1.0.0"}, // no Artifact
	})
	if err == nil {
		t.Fatal("expected abort for required plugin with no artifact")
	}
}

func TestOptionalNoArtifactSkips(t *testing.T) {
	cfg := reconcileCfg(t)
	ok, err := installAll(context.Background(), cfg, []Plugin{
		{PluginID: "demo", GrafanaSlug: "demo-app", Required: false, Version: "1.0.0"}, // no Artifact
	})
	if err != nil {
		t.Fatalf("optional plugin should skip, not error: %v", err)
	}
	if len(ok) != 0 {
		t.Errorf("provisionable = %d, want 0 (skipped)", len(ok))
	}
}

func TestPruneRemovesDroppedPlugin(t *testing.T) {
	tgz, sum := makeTarGz(t, map[string]string{"gpx_demo_linux_amd64": "bin"})
	srv, _ := artifactServer(t, tgz)
	cfg := reconcileCfg(t)
	p := Plugin{
		PluginID: "demo", GrafanaSlug: "demo-app", Required: true, Version: "1.0.0",
		Artifact: &Artifact{DownloadURL: srv.URL, Sha256: sum, SizeBytes: int64(len(tgz))},
	}
	if err := install(context.Background(), cfg, p); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.PluginsDir, "demo-app")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink should exist after install: %v", err)
	}
	// Reconcile with an empty desired set -> the symlink should be pruned.
	if err := prune(cfg, nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected demo-app symlink pruned, stat err = %v", err)
	}
}

// Compile-time guard that the JSON shape decodes (mirrors the control
// plane's instancePluginEntry).
func TestPluginJSONRoundTrip(t *testing.T) {
	in := Plugin{PluginID: "x", GrafanaSlug: "y", Version: "1.0.0",
		Artifact: &Artifact{DownloadURL: "u", Sha256: "s", SizeBytes: 3}}
	b, _ := json.Marshal(in)
	var out Plugin
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Artifact == nil || out.Artifact.Sha256 != "s" {
		t.Errorf("artifact round-trip lost data: %+v", out.Artifact)
	}
}
