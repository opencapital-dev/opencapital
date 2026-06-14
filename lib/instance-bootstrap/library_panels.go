package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// grafanaClient sends authenticated requests to a local Grafana instance.
type grafanaClient struct {
	baseURL string
	auth    grafanaAuth
	hc      *http.Client
}

// grafanaAuth holds the credentials for one auth mode.
// Webauth takes priority over Basic when both are set.
type grafanaAuth struct {
	WebAuthUser string
	BasicUser   string
	BasicPass   string
}

func (a grafanaAuth) apply(req *http.Request) {
	if a.WebAuthUser != "" {
		req.Header.Set("X-WEBAUTH-USER", a.WebAuthUser)
		return
	}
	if a.BasicUser != "" {
		req.SetBasicAuth(a.BasicUser, a.BasicPass)
	}
}

func newGrafanaClient(cfg Config) *grafanaClient {
	return &grafanaClient{
		baseURL: strings.TrimRight(cfg.GrafanaURL, "/"),
		auth: grafanaAuth{
			WebAuthUser: cfg.GrafanaWebAuthUser,
			BasicUser:   cfg.GrafanaBasicUser,
			BasicPass:   cfg.GrafanaBasicPassword,
		},
		hc: &http.Client{Timeout: 15 * time.Second},
	}
}

// waitHealthy polls GET /api/health until it returns 200 or ctx expires.
func (c *grafanaClient) waitHealthy(ctx context.Context) error {
	url := c.baseURL + "/api/health"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		c.auth.apply(req)
		resp, err := c.hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// folderHit is one entry of GET /api/folders and the body of POST /api/folders.
// id is deprecated in Grafana but still the only field that moves an existing
// library element between folders (PATCH honours folderId, not folderUid), so
// we carry both.
type folderHit struct {
	ID    int64  `json:"id"`
	UID   string `json:"uid"`
	Title string `json:"title"`
}

// ensureFolder resolves the folder titled `title`, creating it if absent, and
// returns its uid+id. Library panels share the per-plugin folder the dashboard
// provisioner creates from the plugins/<plugin_id>/ directory name, so both
// resolve the same folder by title (= plugin_id). Either order converges: if we
// create it first the provisioner later adopts it by title; if the provisioner
// created it first we find it here. A provisioner-owned folder is managed as
// "classic-file-provisioning", which the library-element API permits (it only
// rejects "repo"-managed folders).
func (c *grafanaClient) ensureFolder(ctx context.Context, title string) (folderHit, error) {
	folders, err := c.listFolders(ctx)
	if err != nil {
		return folderHit{}, err
	}
	for _, f := range folders {
		if f.Title == title {
			return f, nil
		}
	}
	return c.createFolder(ctx, title)
}

func (c *grafanaClient) listFolders(ctx context.Context) ([]folderHit, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/folders?limit=1000", nil)
	if err != nil {
		return nil, err
	}
	c.auth.apply(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET folders: %s %s", resp.Status, body)
	}
	var out []folderHit
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode folders: %w", err)
	}
	return out, nil
}

func (c *grafanaClient) createFolder(ctx context.Context, title string) (folderHit, error) {
	b, err := json.Marshal(map[string]any{"title": title})
	if err != nil {
		return folderHit{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/folders", bytes.NewReader(b))
	if err != nil {
		return folderHit{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth.apply(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return folderHit{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return folderHit{}, fmt.Errorf("POST folder %q: %s %s", title, resp.Status, rb)
	}
	var out folderHit
	if err := json.Unmarshal(rb, &out); err != nil {
		return folderHit{}, fmt.Errorf("decode created folder %q: %w", title, err)
	}
	return out, nil
}

// libraryElementResult is the API response body for GET /api/library-elements/{uid}.
type libraryElementResult struct {
	Result struct {
		UID       string          `json:"uid"`
		Name      string          `json:"name"`
		Kind      int64           `json:"kind"`
		Model     json.RawMessage `json:"model"`
		Version   int64           `json:"version"`
		FolderUID string          `json:"folderUid"`
	} `json:"result"`
}

// getLibraryElement returns the element for uid, or (nil, nil) when absent (404).
func (c *grafanaClient) getLibraryElement(ctx context.Context, uid string) (*libraryElementResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/library-elements/"+uid, nil)
	if err != nil {
		return nil, err
	}
	c.auth.apply(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET library-element %s: %s %s", uid, resp.Status, body)
	}
	var out libraryElementResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode library-element %s: %w", uid, err)
	}
	return &out, nil
}

// createLibraryElement POSTs a new library element (kind 1 = panel) into
// folderUID. The create path honours folderUid directly (unlike patch).
func (c *grafanaClient) createLibraryElement(ctx context.Context, uid, name, folderUID string, model json.RawMessage) error {
	body := map[string]any{
		"uid":       uid,
		"name":      name,
		"kind":      1,
		"model":     model,
		"folderUid": folderUID,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/library-elements", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth.apply(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST library-element %s: %s %s", uid, resp.Status, rb)
	}
	return nil
}

// patchLibraryElement PATCHes an existing library element, sending the current
// version for optimistic concurrency. Grafana returns 412 on a stale version.
//
// Single-writer assumption: we GET version N then PATCH N. A concurrent writer
// would make this PATCH 412 (stale), or a concurrent create would make the
// create path 400 (already-exists) — either aborts the phase. Acceptable: this
// is a single-writer post-start oneshot, not safe to run concurrently.
func (c *grafanaClient) patchLibraryElement(ctx context.Context, uid, name string, model json.RawMessage, version, folderID int64) error {
	// Grafana's PATCH moves an element between folders via the deprecated numeric
	// folderId (folderUid on PATCH only gates the destination permission check);
	// folder_uid is then derived from folderId server-side. So send folderId to
	// relocate panels created before per-plugin foldering into the right folder.
	body := map[string]any{
		"name":     name,
		"kind":     1,
		"model":    model,
		"version":  version,
		"folderId": folderID,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		c.baseURL+"/api/library-elements/"+uid, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth.apply(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PATCH library-element %s: %s %s", uid, resp.Status, rb)
	}
	return nil
}

// pluginDisplayName reads the "name" field from the installed bundle's plugin.json.
// Falls back to pluginID on any read/parse error.
func pluginDisplayName(pluginsDir, slug, pluginID string) string {
	b, err := os.ReadFile(filepath.Join(pluginsDir, slug, "plugin.json"))
	if err != nil {
		return pluginID
	}
	var m struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &m) != nil || m.Name == "" {
		return pluginID
	}
	return m.Name
}

// validShortUID matches Grafana's IsValidShortUID charset: a-z A-Z 0-9 - _
var validShortUID = regexp.MustCompile(`^[a-zA-Z0-9\-_]+$`)

// libraryPanelUID derives and validates the Grafana UID for a plugin's panel file.
// uid = "<plugin_id>-<stem>", max 40 chars, charset a-zA-Z0-9\-_.
// Returns an error rather than silently truncating — UID stability is a contract.
func libraryPanelUID(pluginID, stem string) (string, error) {
	uid := pluginID + "-" + stem
	if len(uid) > 40 {
		return "", fmt.Errorf("derived UID %q is %d chars, max 40 — shorten plugin_id or panel stem (UID stability is a contract; do not truncate)", uid, len(uid))
	}
	if !validShortUID.MatchString(uid) {
		return "", fmt.Errorf("derived UID %q contains characters not in [a-zA-Z0-9-_] — fix plugin_id or panel stem", uid)
	}
	return uid, nil
}

// modelsEqual compares two JSON objects semantically by normalising each
// through unmarshal+marshal, so whitespace and key-order differences don't
// produce false positives.
func modelsEqual(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	na, _ := json.Marshal(va)
	nb, _ := json.Marshal(vb)
	return bytes.Equal(na, nb)
}

// applyGrafanaModelDefaults mirrors Grafana's syncFieldsWithModel
// (libraryelements/database.go), which runs on BOTH create and patch and
// injects `type` and `description` into the stored model when absent (each
// defaulting to "" for a freshly created element). GET returns that mutated
// model, so to be idempotent we must compare against the mutated form: we apply
// the same defaulting to the desired (file) model before comparing. This
// deliberately couples to Grafana's store behavior — keep it in sync if the
// vendored libraryelements source changes.
func applyGrafanaModelDefaults(file json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(file, &m) != nil || m == nil {
		return file // non-object or unparseable: leave as-is, modelsEqual handles it
	}
	if _, ok := m["type"]; !ok {
		m["type"] = ""
	}
	if _, ok := m["description"]; !ok {
		m["description"] = ""
	}
	out, err := json.Marshal(m)
	if err != nil {
		return file
	}
	return out
}

// ProvisionLibraryPanels upserts every installed plugin's library panels into
// the local Grafana instance. It waits for Grafana to be healthy before
// touching the API, then for each plugin reads library-panels/*.json and
// creates or patches the corresponding library_element by UID.
//
// Library elements are org-scoped (org 1 in a single-org instance). No
// pruning/deletion is performed: orphaned library panels are left in place
// because Grafana cannot hard-delete connected ones.
//
// Each plugin's panels go into a folder titled plugin_id — the same folder its
// provisioned dashboards use — so the panel picker can filter by plugin. UIDs
// stay plugin-prefixed (stable id), but the display name is the bare stem.
func ProvisionLibraryPanels(ctx context.Context, cfg Config) error {
	if err := validateLibraryPanels(cfg); err != nil {
		return err
	}
	plugins, err := Fetch(ctx, cfg)
	if err != nil {
		return fmt.Errorf("fetch plugins: %w", err)
	}

	gc := newGrafanaClient(cfg)
	emit(cfg, progressEvent{Event: "library-panels", Status: "waiting-grafana"})
	if err := gc.waitHealthy(ctx); err != nil {
		return fmt.Errorf("wait for Grafana health: %w", err)
	}
	emit(cfg, progressEvent{Event: "library-panels", Status: "grafana-ready"})

	for _, p := range plugins {
		if p.GrafanaSlug == "" {
			continue
		}
		lpDir := filepath.Join(cfg.PluginsDir, p.GrafanaSlug, "library-panels")
		entries, err := os.ReadDir(lpDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // plugin ships no library-panels/
			}
			return fmt.Errorf("read library-panels dir for %s: %w", p.PluginID, err)
		}

		var panels []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				panels = append(panels, e.Name())
			}
		}
		if len(panels) == 0 {
			continue
		}

		// Panels live in a per-plugin folder titled by the plugin's display name —
		// the same folder provisioned dashboards land in. Name uniqueness is scoped
		// per folder_uid, so bare stems can't collide across plugins.
		folderTitle := pluginDisplayName(cfg.PluginsDir, p.GrafanaSlug, p.PluginID)
		panelFolder, err := gc.ensureFolder(ctx, folderTitle)
		if err != nil {
			return fmt.Errorf("ensure folder for %s: %w", p.PluginID, err)
		}

		for _, fname := range panels {
			stem := strings.TrimSuffix(fname, ".json")
			uid, err := libraryPanelUID(p.PluginID, stem)
			if err != nil {
				return fmt.Errorf("plugin %s panel %s: %w", p.PluginID, stem, err)
			}
			modelBytes, err := os.ReadFile(filepath.Join(lpDir, fname))
			if err != nil {
				return fmt.Errorf("read panel %s/%s: %w", p.PluginID, fname, err)
			}
			var panelMeta struct {
				Title string `json:"title"`
			}
			name := stem
			if json.Unmarshal(modelBytes, &panelMeta) == nil && panelMeta.Title != "" {
				name = panelMeta.Title
			}

			// Surface the dashboard variables a metric-ref target uses (declared
			// server-side in the metric's @bind selectors) onto the stored query,
			// so Grafana re-runs the panel when those variables change.
			modelBytes = injectMetricVarDeps(modelBytes, cfg.PluginsDir)

			existing, err := gc.getLibraryElement(ctx, uid)
			if err != nil {
				return fmt.Errorf("get library-element %s: %w", uid, err)
			}

			if existing == nil {
				if err := gc.createLibraryElement(ctx, uid, name, panelFolder.UID, modelBytes); err != nil {
					return fmt.Errorf("create library-element %s: %w", uid, err)
				}
				emit(cfg, progressEvent{Event: "library-panel", Plugin: p.PluginID, Status: "created", Detail: uid})
				continue
			}

			if modelsEqual(existing.Result.Model, applyGrafanaModelDefaults(modelBytes)) &&
				existing.Result.Name == name && existing.Result.FolderUID == panelFolder.UID {
				emit(cfg, progressEvent{Event: "library-panel", Plugin: p.PluginID, Status: "no-op", Detail: uid})
				continue
			}

			if err := gc.patchLibraryElement(ctx, uid, name, modelBytes, existing.Result.Version, panelFolder.ID); err != nil {
				return fmt.Errorf("patch library-element %s: %w", uid, err)
			}
			emit(cfg, progressEvent{Event: "library-panel", Plugin: p.PluginID, Status: "patched", Detail: uid})
		}
	}
	return nil
}
