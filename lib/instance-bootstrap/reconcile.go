package bootstrap

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// markerFile records the artifact sha256 an installed cache entry holds, so
// a re-run with a matching artifact skips the download + extract.
const markerFile = ".artifact-sha256"

// hostPlatform returns cfg.Platform or, when empty, the running host's
// "<os>-<arch>" tag (e.g. "darwin-arm64").
func hostPlatform(cfg Config) string {
	if cfg.Platform != "" {
		return cfg.Platform
	}
	return runtime.GOOS + "-" + runtime.GOARCH
}

// progressEvent is one NDJSON line emitted for the desktop shell.
type progressEvent struct {
	Event  string `json:"event"`
	Plugin string `json:"plugin,omitempty"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func emit(cfg Config, ev progressEvent) {
	w := cfg.ProgressW
	if w == nil {
		w = os.Stdout
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

// installAll installs each plugin's host-platform binary and returns the
// subset that is provisionable (binary present on disk). A required plugin
// with no artifact, or one that fails to install, aborts the whole
// reconcile. An optional plugin in the same situation is skipped with a
// warning so one bad optional plugin doesn't brick the instance.
func installAll(ctx context.Context, cfg Config, plugins []Plugin) ([]Plugin, error) {
	var ok []Plugin
	for _, p := range plugins {
		if p.GrafanaSlug == "" {
			continue // control plane should filter; defend anyway
		}
		if p.Artifact == nil {
			if p.Required {
				return nil, fmt.Errorf("required plugin %q has no artifact for platform %s", p.PluginID, hostPlatform(cfg))
			}
			emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "skipped", Detail: "no artifact for platform"})
			continue
		}
		if err := install(ctx, cfg, p); err != nil {
			if p.Required {
				return nil, fmt.Errorf("install required plugin %q: %w", p.PluginID, err)
			}
			emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "skipped", Detail: err.Error()})
			continue
		}
		ok = append(ok, p)
	}
	return ok, nil
}

// install ensures one plugin's binary is present in the cache and symlinked
// into the plugins dir. Idempotent: an existing cache entry whose marker
// matches the artifact sha is reused without re-downloading.
func install(ctx context.Context, cfg Config, p Plugin) error {
	platform := hostPlatform(cfg)
	cacheDir := filepath.Join(cfg.PluginCacheDir, p.PluginID, p.Version, platform)
	linkPath := filepath.Join(cfg.PluginsDir, p.GrafanaSlug)

	if cacheValid(cacheDir, p.Artifact.Sha256) {
		emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "cached"})
		// Re-assert the backend exec bit: a cache entry extracted before this
		// fix (or by an older build) still has the gpx_* binary at 0644.
		if err := ensureBackendExecutable(cacheDir); err != nil {
			return err
		}
		return linkPlugin(linkPath, cacheDir)
	}

	emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "downloading"})
	tmpFile, err := download(ctx, cfg, p.Artifact)
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile)

	emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "verifying"})
	sum, err := fileSHA256(tmpFile)
	if err != nil {
		return err
	}
	if sum != p.Artifact.Sha256 {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", sum, p.Artifact.Sha256)
	}

	emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "extracting"})
	if err := extractInto(tmpFile, cacheDir); err != nil {
		return err
	}
	if err := ensureBackendExecutable(cacheDir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cacheDir, markerFile), []byte(p.Artifact.Sha256), 0o644); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}

	emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "linking"})
	if err := linkPlugin(linkPath, cacheDir); err != nil {
		return err
	}
	emit(cfg, progressEvent{Event: "plugin", Plugin: p.PluginID, Status: "done"})
	return nil
}

// ensureBackendExecutable sets the exec bit on a backend plugin's binary.
// OCI plugin artifacts pack the gpx_* backend binary with tar mode 0644, which
// extractInto faithfully preserves — so Grafana's fork/exec of the backend
// fails with "permission denied" and the plugin is reported "not installed".
// Grafana names the backend file <executable>_<os>_<arch>[.exe], so chmod +x
// every file whose name starts with the plugin.json "executable" prefix. No-op
// for frontend-only plugins (backend=false or no executable).
func ensureBackendExecutable(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if os.IsNotExist(err) {
		return nil // no manifest → nothing to mark executable (Grafana validates plugin.json)
	}
	if err != nil {
		return fmt.Errorf("read plugin.json: %w", err)
	}
	var pj struct {
		Backend    bool   `json:"backend"`
		Executable string `json:"executable"`
	}
	if err := json.Unmarshal(b, &pj); err != nil {
		return fmt.Errorf("parse plugin.json: %w", err)
	}
	if !pj.Backend || pj.Executable == "" {
		return nil
	}
	prefix := filepath.Base(pj.Executable)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Chmod(path, 0o755); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	return nil
}

// cacheValid reports whether cacheDir holds an extracted plugin whose
// marker matches sha.
func cacheValid(cacheDir, sha string) bool {
	got, err := os.ReadFile(filepath.Join(cacheDir, markerFile))
	return err == nil && strings.TrimSpace(string(got)) == sha
}

// download streams the artifact to a temp file and returns its path.
func download(ctx context.Context, cfg Config, art *Artifact) (string, error) {
	timeout := cfg.HTTPTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	client := &http.Client{Timeout: timeout}
	resp, err := blobGet(ctx, client, art.DownloadURL, "")
	if err != nil {
		return "", fmt.Errorf("download %s: %w", art.DownloadURL, err)
	}
	// zot may be bearer-gated (token-auth registry). On a 401 it returns a
	// WWW-Authenticate: Bearer challenge naming the realm (control-plane's
	// /v2/token broker). Follow it with no credentials — the broker mints an
	// anonymous pull token — then retry once with that bearer.
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		token, terr := fetchRegistryToken(ctx, client, challenge)
		if terr != nil {
			return "", fmt.Errorf("download %s: auth: %w", art.DownloadURL, terr)
		}
		resp, err = blobGet(ctx, client, art.DownloadURL, token)
		if err != nil {
			return "", fmt.Errorf("download %s: %w", art.DownloadURL, err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", art.DownloadURL, resp.Status)
	}
	f, err := os.CreateTemp("", "plugin-*.tar.gz")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, resp.Body)
	cerr := f.Close()
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if cerr != nil {
		os.Remove(f.Name())
		return "", cerr
	}
	return f.Name(), nil
}

// blobGet issues a GET, optionally with a bearer token. The caller closes the
// response body.
func blobGet(ctx context.Context, client *http.Client, rawURL, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req)
}

// fetchRegistryToken parses a Docker-v2 `WWW-Authenticate: Bearer
// realm="...",service="...",scope="..."` challenge, GETs the realm with the
// given service+scope (no credentials — control-plane's broker mints an
// anonymous pull token), and returns the token. Reads `token` or, per the
// spec, `access_token`.
func fetchRegistryToken(ctx context.Context, client *http.Client, challenge string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(challenge, prefix) {
		return "", fmt.Errorf("unexpected challenge %q", challenge)
	}
	params := map[string]string{}
	for _, part := range splitChallenge(challenge[len(prefix):]) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		params[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("challenge has no realm: %q", challenge)
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("bad realm %q: %w", realm, err)
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if s := params["scope"]; s != "" {
		q.Set("scope", s)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s: %s", u.String(), resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint returned no token")
}

// splitChallenge splits the comma-separated params of a Bearer challenge,
// respecting double-quoted values (a scope value may contain commas).
func splitChallenge(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractInto unpacks a verified .tar.gz into dest atomically: it extracts
// to a sibling temp dir, removes any prior dest, then renames into place.
// Guards against zip-slip — entries that would escape dest are rejected.
func extractInto(tarball, dest string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	if err := untar(tarball, staging); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.Rename(staging, dest); err != nil {
		return fmt.Errorf("publish %s: %w", dest, err)
	}
	return nil
}

func untar(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // size bounded by trusted, sha-verified artifact
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// Skip symlinks, devices, etc. Plugin tarballs are plain files.
		}
	}
	return nil
}

// safeJoin joins name onto dir, rejecting paths that escape dir (zip-slip).
func safeJoin(dir, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("unsafe absolute tar entry %q", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe tar entry %q escapes target", name)
	}
	target := filepath.Join(dir, clean)
	if target != dir && !strings.HasPrefix(target, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe tar entry %q escapes target", name)
	}
	return target, nil
}

// linkPlugin points linkPath at cacheDir via a symlink, replacing any
// existing entry. On platforms without symlink support this would need a
// copy fallback; v0 desktop targets macOS/Linux.
func linkPlugin(linkPath, cacheDir string) error {
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(linkPath); err == nil {
		// Already correct? Leave it.
		if fi.Mode()&os.ModeSymlink != 0 {
			if dst, _ := os.Readlink(linkPath); dst == cacheDir {
				return nil
			}
		}
		if err := os.RemoveAll(linkPath); err != nil {
			return err
		}
	}
	return os.Symlink(cacheDir, linkPath)
}

// prune removes plugins-dir symlinks that point into our cache but whose
// slug is no longer in the desired set (the plugin was uninstalled). Only
// our own managed symlinks are touched; unrelated entries are left alone.
func prune(cfg Config, desired []Plugin) error {
	want := make(map[string]bool, len(desired))
	for _, p := range desired {
		if p.GrafanaSlug != "" {
			want[p.GrafanaSlug] = true
		}
	}
	entries, err := os.ReadDir(cfg.PluginsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cacheAbs, _ := filepath.Abs(cfg.PluginCacheDir)
	for _, e := range entries {
		if want[e.Name()] {
			continue
		}
		linkPath := filepath.Join(cfg.PluginsDir, e.Name())
		fi, err := os.Lstat(linkPath)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue // not a symlink we manage
		}
		dst, err := os.Readlink(linkPath)
		if err != nil {
			continue
		}
		if dstAbs, _ := filepath.Abs(dst); strings.HasPrefix(dstAbs, cacheAbs) {
			if err := os.Remove(linkPath); err != nil {
				return err
			}
			emit(cfg, progressEvent{Event: "plugin", Plugin: e.Name(), Status: "pruned"})
		}
	}
	return nil
}
