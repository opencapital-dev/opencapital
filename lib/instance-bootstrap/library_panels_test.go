package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeGrafana models the Grafana library-elements + folders API in memory.
// Elements are tracked by uid. POST creates with version 1, PATCH bumps version.
// Folders are tracked by title with a deterministic uid ("folder-"+title) so
// tests can pre-seed an element's folderUID to match what ensureFolder resolves.
type fakeGrafana struct {
	mu       sync.Mutex
	elements map[string]fakeElement
	folders  map[string]fakeFolder // by title
	folderID int64                 // autoincrement
	// healthReady gates /api/health: when >= 0 it counts down before returning 200.
	healthCalls int32 // total calls
	healthReady int32 // calls to serve 503 before 200
}

type fakeElement struct {
	uid       string
	name      string
	model     json.RawMessage
	version   int64
	folderUID string
}

type fakeFolder struct {
	id    int64
	uid   string
	title string
}

func newFakeGrafana(readyAfter int) *fakeGrafana {
	return &fakeGrafana{
		elements:    map[string]fakeElement{},
		folders:     map[string]fakeFolder{},
		healthReady: int32(readyAfter),
	}
}

// syncFieldsWithModel replicates Grafana's libraryelements store mutation: on
// both create and patch it injects `type` and `description` (each defaulting to
// "" when absent) into the model before persisting. The real GET therefore
// returns a model that is NOT byte-equal to the file model, so the test double
// must replicate this or the no-op test would pass against a lenient fake.
func syncFieldsWithModel(in json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(in, &m) != nil || m == nil {
		return in
	}
	if _, ok := m["type"]; !ok {
		m["type"] = ""
	}
	if _, ok := m["description"]; !ok {
		m["description"] = ""
	}
	out, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return out
}

func (f *fakeGrafana) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&f.healthCalls, 1)
		if n <= atomic.LoadInt32(&f.healthReady) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"database":"ok"}`))
	})

	mux.HandleFunc("/api/library-elements/", func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.URL.Path, "/api/library-elements/")
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			el, ok := f.elements[uid]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			resp := map[string]any{
				"result": map[string]any{
					"uid":       el.uid,
					"name":      el.name,
					"kind":      1,
					"model":     el.model,
					"version":   el.version,
					"folderUid": el.folderUID,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case http.MethodPatch:
			var cmd struct {
				Name     string          `json:"name"`
				Kind     int64           `json:"kind"`
				Model    json.RawMessage `json:"model"`
				Version  int64           `json:"version"`
				FolderID int64           `json:"folderId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			el, ok := f.elements[uid]
			if !ok {
				f.mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if cmd.Version != el.version {
				f.mu.Unlock()
				w.WriteHeader(http.StatusPreconditionFailed) // 412: stale version
				return
			}
			el.name = cmd.Name
			el.model = syncFieldsWithModel(cmd.Model)
			// Mirror Grafana: PATCH moves via folderId; folder_uid is derived from it.
			el.folderUID = f.folderUIDByIDLocked(cmd.FolderID)
			el.version++
			f.elements[uid] = el
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"uid": uid}})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/library-elements", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var cmd struct {
			UID       string          `json:"uid"`
			Name      string          `json:"name"`
			Kind      int64           `json:"kind"`
			Model     json.RawMessage `json:"model"`
			FolderUID string          `json:"folderUid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, exists := f.elements[cmd.UID]; exists {
			http.Error(w, "already exists", http.StatusBadRequest)
			return
		}
		f.elements[cmd.UID] = fakeElement{
			uid: cmd.UID, name: cmd.Name, model: syncFieldsWithModel(cmd.Model),
			version: 1, folderUID: cmd.FolderUID,
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"uid": cmd.UID}})
	})

	// Folders API: GET lists, POST creates a folder with a deterministic uid so
	// tests can pre-seed an element's folderUID. ensureFolder GETs then POSTs only
	// when absent, so POST is effectively find-or-create here too.
	mux.HandleFunc("/api/folders", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			out := make([]map[string]any, 0, len(f.folders))
			for _, fl := range f.folders {
				out = append(out, map[string]any{"id": fl.id, "uid": fl.uid, "title": fl.title})
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)

		case http.MethodPost:
			var cmd struct {
				Title string `json:"title"`
			}
			if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			fl, ok := f.folders[cmd.Title]
			if !ok {
				f.folderID++
				fl = fakeFolder{id: f.folderID, uid: "folder-" + cmd.Title, title: cmd.Title}
				f.folders[cmd.Title] = fl
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fl.id, "uid": fl.uid, "title": fl.title})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	return mux
}

// folderUIDByIDLocked maps a numeric folder id back to its uid. Caller holds f.mu.
func (f *fakeGrafana) folderUIDByIDLocked(id int64) string {
	for _, fl := range f.folders {
		if fl.id == id {
			return fl.uid
		}
	}
	return ""
}

// fakeControlPlane serves the minimal control-plane responses that Fetch needs.
// It returns a fixed plugin list and no-op versions/artifact endpoints.
func fakeControlPlane(t *testing.T, plugins []Plugin) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/orgs/") && strings.HasSuffix(r.URL.Path, "/plugins"):
			_ = json.NewEncoder(w).Encode(plugins)
		case strings.Contains(r.URL.Path, "/versions") && !strings.Contains(r.URL.Path, "/artifact"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"versions": []VersionStatus{{Version: "v1.0.0", Validated: true}},
			})
		case strings.Contains(r.URL.Path, "/artifact"):
			_ = json.NewEncoder(w).Encode(Artifact{
				DownloadURL: "http://unused/blob", Sha256: "abc", SizeBytes: 1,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// seedLibraryPanel creates <PluginsDir>/<slug>/library-panels/<file> with content.
func seedLibraryPanel(t *testing.T, cfg Config, slug, file, content string) {
	t.Helper()
	dir := filepath.Join(cfg.PluginsDir, slug, "library-panels")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seed library-panels dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatalf("seed library-panel file: %v", err)
	}
}

func libCfg(t *testing.T, cpURL, grafanaURL string) Config {
	t.Helper()
	return Config{
		OrgID:                 testOrgID,
		ControlPlaneURL:       cpURL,
		BootstrapToken:        testToken,
		ProvisioningDir:       t.TempDir(),
		PluginControlPlaneURL: "http://cp",
		PluginGatewayURL:      "http://gw",
		Platform:              "linux-amd64",
		PluginCacheDir:        t.TempDir(),
		PluginsDir:            t.TempDir(),
		GrafanaURL:            grafanaURL,
		GrafanaWebAuthUser:    "admin",
	}
}

func TestProvisionLibraryPanels_CreateWhenAbsent(t *testing.T) {
	fg := newFakeGrafana(0)
	gs := httptest.NewServer(fg.handler())
	defer gs.Close()

	plugins := []Plugin{
		{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"},
	}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, gs.URL)
	const model = `{"type":"timeseries","title":"My Panel"}`
	seedLibraryPanel(t, cfg, "pm-myplugin-app", "overview.json", model)

	if err := ProvisionLibraryPanels(context.Background(), cfg); err != nil {
		t.Fatalf("ProvisionLibraryPanels: %v", err)
	}

	fg.mu.Lock()
	el, ok := fg.elements["myplugin-overview"]
	fg.mu.Unlock()
	if !ok {
		t.Fatal("element myplugin-overview not created")
	}
	if el.name != "My Panel" {
		t.Errorf("name = %q, want My Panel (title from panel JSON)", el.name)
	}
	if el.folderUID != "folder-myplugin" {
		t.Errorf("folderUID = %q, want folder-myplugin (per-plugin folder; no plugin.json seeded so pluginID is the title)", el.folderUID)
	}
	if el.version != 1 {
		t.Errorf("version = %d, want 1", el.version)
	}
}

func TestProvisionLibraryPanels_PatchWhenModelChanged(t *testing.T) {
	fg := newFakeGrafana(0)
	fg.elements["myplugin-overview"] = fakeElement{
		uid:       "myplugin-overview",
		name:      "Old",
		model:     json.RawMessage(`{"type":"timeseries","title":"Old"}`),
		version:   3,
		folderUID: "folder-myplugin",
	}
	gs := httptest.NewServer(fg.handler())
	defer gs.Close()

	plugins := []Plugin{
		{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"},
	}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, gs.URL)
	seedLibraryPanel(t, cfg, "pm-myplugin-app", "overview.json", `{"type":"timeseries","title":"New"}`)

	if err := ProvisionLibraryPanels(context.Background(), cfg); err != nil {
		t.Fatalf("ProvisionLibraryPanels: %v", err)
	}

	fg.mu.Lock()
	el := fg.elements["myplugin-overview"]
	fg.mu.Unlock()
	if el.version != 4 {
		t.Errorf("version = %d, want 4 (patched from 3)", el.version)
	}
	var m map[string]any
	_ = json.Unmarshal(el.model, &m)
	if m["title"] != "New" {
		t.Errorf("model title = %v, want New", m["title"])
	}
}

func TestProvisionLibraryPanels_NoOpWhenIdentical(t *testing.T) {
	fg := newFakeGrafana(0)
	// Stored model carries the `description:""` Grafana injects on store; the file
	// below OMITS description. The reconcile must still recognise these as equal
	// (via applyGrafanaModelDefaults) and NOT patch — proving idempotency against
	// the real store contract, not a lenient fake.
	fg.elements["myplugin-overview"] = fakeElement{
		uid:       "myplugin-overview",
		name:      "Same",
		model:     json.RawMessage(`{"type":"timeseries","title":"Same","description":""}`),
		version:   2,
		folderUID: "folder-myplugin",
	}
	gs := httptest.NewServer(fg.handler())
	defer gs.Close()

	plugins := []Plugin{
		{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"},
	}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, gs.URL)
	// File omits description and uses extra whitespace — must still be a no-op.
	seedLibraryPanel(t, cfg, "pm-myplugin-app", "overview.json", `{  "type" : "timeseries", "title":"Same"  }`)

	if err := ProvisionLibraryPanels(context.Background(), cfg); err != nil {
		t.Fatalf("ProvisionLibraryPanels: %v", err)
	}

	fg.mu.Lock()
	el := fg.elements["myplugin-overview"]
	fg.mu.Unlock()
	if el.version != 2 {
		t.Errorf("version = %d, want 2 (no-op; should not have patched)", el.version)
	}
}

// TestProvisionLibraryPanels_CreateThenRerunNoOp is the regression that would
// have caught the idempotency bug: after a real create (which injects the
// defaulted keys into the stored model), a second run with the SAME file must
// be a no-op. The file omits description; the stored model gained it on create.
func TestProvisionLibraryPanels_CreateThenRerunNoOp(t *testing.T) {
	fg := newFakeGrafana(0)
	gs := httptest.NewServer(fg.handler())
	defer gs.Close()

	plugins := []Plugin{
		{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"},
	}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, gs.URL)
	// File deliberately omits `description` (the field Grafana injects on store).
	seedLibraryPanel(t, cfg, "pm-myplugin-app", "overview.json", `{"type":"timeseries","title":"Same"}`)

	if err := ProvisionLibraryPanels(context.Background(), cfg); err != nil {
		t.Fatalf("first run (create): %v", err)
	}
	fg.mu.Lock()
	v1 := fg.elements["myplugin-overview"].version
	fg.mu.Unlock()
	if v1 != 1 {
		t.Fatalf("after create, version = %d, want 1", v1)
	}

	if err := ProvisionLibraryPanels(context.Background(), cfg); err != nil {
		t.Fatalf("second run (re-run): %v", err)
	}
	fg.mu.Lock()
	v2 := fg.elements["myplugin-overview"].version
	fg.mu.Unlock()
	if v2 != 1 {
		t.Errorf("after re-run, version = %d, want 1 (re-run must be a no-op, not a patch)", v2)
	}
}

func TestProvisionLibraryPanels_DerivedUIDAndName(t *testing.T) {
	fg := newFakeGrafana(0)
	gs := httptest.NewServer(fg.handler())
	defer gs.Close()

	plugins := []Plugin{
		{PluginID: "core-app", GrafanaSlug: "pm-core-app", Type: "app"},
	}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, gs.URL)
	seedLibraryPanel(t, cfg, "pm-core-app", "pnl-chart.json", `{"type":"timeseries"}`)

	if err := ProvisionLibraryPanels(context.Background(), cfg); err != nil {
		t.Fatalf("ProvisionLibraryPanels: %v", err)
	}

	fg.mu.Lock()
	el, ok := fg.elements["core-app-pnl-chart"]
	fg.mu.Unlock()
	if !ok {
		t.Fatal("element core-app-pnl-chart not found")
	}
	if el.name != "pnl-chart" {
		t.Errorf("name = %q, want pnl-chart (stem fallback; model has no title field)", el.name)
	}
}

func TestProvisionLibraryPanels_IllegalUIDFailsLoudly(t *testing.T) {
	// A plugin_id + stem that exceeds 40 chars must fail loudly (not silently truncate).
	fg := newFakeGrafana(0)
	gs := httptest.NewServer(fg.handler())
	defer gs.Close()

	// plugin_id = 20 chars, stem = 21 chars → uid = 42 chars > 40 limit
	longID := "twentycharspluginid0"
	longStem := "twentyonecharstemname"
	if len(longID+"-"+longStem) <= 40 {
		t.Skipf("test precondition: uid %q is not > 40 chars; adjust", longID+"-"+longStem)
	}

	plugins := []Plugin{
		{PluginID: longID, GrafanaSlug: "pm-long-app", Type: "app"},
	}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, gs.URL)
	seedLibraryPanel(t, cfg, "pm-long-app", longStem+".json", `{"type":"timeseries"}`)

	err := ProvisionLibraryPanels(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for over-long UID, got nil")
	}
	if !strings.Contains(err.Error(), "40") {
		t.Errorf("error should mention max 40 chars: %v", err)
	}
}

func TestProvisionLibraryPanels_NoLibraryPanelsDirSkipped(t *testing.T) {
	fg := newFakeGrafana(0)
	gs := httptest.NewServer(fg.handler())
	defer gs.Close()

	plugins := []Plugin{
		{PluginID: "nodash", GrafanaSlug: "pm-nodash-app", Type: "app"},
	}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, gs.URL)
	// Create slug dir but no library-panels/ subdir.
	if err := os.MkdirAll(filepath.Join(cfg.PluginsDir, "pm-nodash-app"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ProvisionLibraryPanels(context.Background(), cfg); err != nil {
		t.Fatalf("ProvisionLibraryPanels should not error when no library-panels dir: %v", err)
	}
	fg.mu.Lock()
	n := len(fg.elements)
	fg.mu.Unlock()
	if n != 0 {
		t.Errorf("expected no elements created, got %d", n)
	}
}

func TestProvisionLibraryPanels_HealthWait(t *testing.T) {
	// Server returns 503 for the first 2 calls, then 200.
	fg := newFakeGrafana(2)
	gs := httptest.NewServer(fg.handler())
	defer gs.Close()

	plugins := []Plugin{
		{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"},
	}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, gs.URL)
	seedLibraryPanel(t, cfg, "pm-myplugin-app", "panel.json", `{"type":"stat"}`)

	if err := ProvisionLibraryPanels(context.Background(), cfg); err != nil {
		t.Fatalf("ProvisionLibraryPanels: %v", err)
	}

	calls := atomic.LoadInt32(&fg.healthCalls)
	if calls < 3 {
		t.Errorf("health endpoint called %d times, expected at least 3 (2 non-ready + 1 ready)", calls)
	}

	fg.mu.Lock()
	_, ok := fg.elements["myplugin-panel"]
	fg.mu.Unlock()
	if !ok {
		t.Error("element not created after health wait")
	}
}

func TestLibraryPanelUID_Valid(t *testing.T) {
	uid, err := libraryPanelUID("core-app", "pnl-chart")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uid != "core-app-pnl-chart" {
		t.Errorf("uid = %q, want core-app-pnl-chart", uid)
	}
}

func TestLibraryPanelUID_TooLong(t *testing.T) {
	// 20 + 1 (hyphen) + 20 = 41 > 40
	_, err := libraryPanelUID("aaaabbbbccccddddeeee", "ffffgggghhhhiiiijjjj")
	if err == nil {
		t.Fatal("expected error for over-long UID")
	}
	if !strings.Contains(err.Error(), "40") {
		t.Errorf("error should mention 40 chars: %v", err)
	}
}

func TestLibraryPanelUID_IllegalChars(t *testing.T) {
	// colon is invalid per Grafana's IsValidShortUID
	_, err := libraryPanelUID("myplugin", "has:colon")
	if err == nil {
		t.Fatal("expected error for illegal char in UID")
	}
	if !strings.Contains(err.Error(), "[a-zA-Z0-9-_]") {
		t.Errorf("error should mention charset: %v", err)
	}
}

func TestModelsEqual_WhitespaceInsensitive(t *testing.T) {
	a := json.RawMessage(`{"type":"timeseries","title":"X"}`)
	b := json.RawMessage(`{  "type" :  "timeseries" ,   "title"  :  "X"  }`)
	if !modelsEqual(a, b) {
		t.Error("expected models equal (whitespace differences)")
	}
}

func TestModelsEqual_DifferentValues(t *testing.T) {
	a := json.RawMessage(`{"title":"A"}`)
	b := json.RawMessage(`{"title":"B"}`)
	if modelsEqual(a, b) {
		t.Error("expected models NOT equal (different values)")
	}
}

func TestGrafanaAuth_WebAuthPreferredOverBasic(t *testing.T) {
	var gotWebAuth, gotBasicAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWebAuth = r.Header.Get("X-WEBAUTH-USER")
		gotBasicAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	gc := &grafanaClient{
		baseURL: srv.URL,
		auth: grafanaAuth{
			WebAuthUser: "admin",
			BasicUser:   "grafana-user",
			BasicPass:   "secret",
		},
		hc: &http.Client{},
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	gc.auth.apply(req)
	_, _ = gc.hc.Do(req)

	if gotWebAuth != "admin" {
		t.Errorf("X-WEBAUTH-USER = %q, want admin", gotWebAuth)
	}
	if gotBasicAuth != "" {
		t.Errorf("Authorization should be absent when webauth is set, got %q", gotBasicAuth)
	}
}

// TestProvisionLibraryPanels_GetNon404IsError proves a non-404 GET failure
// (500/401) is surfaced as an error and does NOT get masked as "absent" — i.e.
// no create is attempted. Guards the "don't mask auth/server failure" property.
func TestProvisionLibraryPanels_GetNon404IsError(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var postCalled int32
			gs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/health":
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"database":"ok"}`))
				// Serve folders OK so the run reaches the library-element GET — the
				// path under test. Without this, ensureFolder's GET would absorb the
				// failure first and the library-element GET would never be exercised.
				case r.URL.Path == "/api/folders" && r.Method == http.MethodGet:
					_, _ = w.Write([]byte(`[]`))
				case r.URL.Path == "/api/folders" && r.Method == http.MethodPost:
					_, _ = w.Write([]byte(`{"id":1,"uid":"folder-myplugin","title":"myplugin"}`))
				case r.Method == http.MethodPost:
					atomic.AddInt32(&postCalled, 1)
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet:
					w.WriteHeader(status) // health-OK but library-element GET fails
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer gs.Close()

			plugins := []Plugin{
				{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"},
			}
			cp := fakeControlPlane(t, plugins)
			defer cp.Close()

			cfg := libCfg(t, cp.URL, gs.URL)
			seedLibraryPanel(t, cfg, "pm-myplugin-app", "overview.json", `{"type":"timeseries"}`)

			err := ProvisionLibraryPanels(context.Background(), cfg)
			if err == nil {
				t.Fatalf("expected error on GET %d, got nil", status)
			}
			if n := atomic.LoadInt32(&postCalled); n != 0 {
				t.Errorf("create was attempted %d times after a %d GET; non-404 must not be treated as absent", n, status)
			}
		})
	}
}

func TestProvisionLibraryPanels_EmptyGrafanaURLFailsValidation(t *testing.T) {
	plugins := []Plugin{{PluginID: "myplugin", GrafanaSlug: "pm-myplugin-app", Type: "app"}}
	cp := fakeControlPlane(t, plugins)
	defer cp.Close()

	cfg := libCfg(t, cp.URL, "") // empty GrafanaURL
	err := ProvisionLibraryPanels(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for empty GrafanaURL, got nil")
	}
	if !strings.Contains(err.Error(), "GrafanaURL is required") {
		t.Errorf("error should name GrafanaURL: %v", err)
	}
}
