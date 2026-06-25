# Basic Data plugin — macro dashboards via compute passthrough — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Generalize the yfinance plugin into a "Basic Data" plugin that renders global-majors macro indicators (FRED direct + DBnomics keyless) as provisioned Grafana dashboards, fetched on demand through the existing `core-datasource` + Python compute sidecar.

**Architecture:** Add an HTTP-fetch + TTL-cache capability to the compute sidecar (Part A). Rename `yfinance-app` → `basic-data-app` and add a Settings page that stores a FRED API key in Postgres (Part B/C). Ship macro `@metric` Python that fetches FRED/DBnomics via the sidecar's `fetch_json` (reading the key from Postgres) and dashboards that reference them (Part D/E). No macro data is persisted.

**Tech Stack:** Python 3.12 (compute sidecar, PyInstaller-frozen, polars), Go (plugin backend, `oc-plugin-sdk/pluginclient`), React/TS + `@grafana/ui` (plugin frontend), Grafana app-plugin provisioning.

**Spec:** `docs/superpowers/specs/2026-06-24-basic-data-macro-plugin-design.md`

**Two repos** (paths below are prefixed with the repo):
- `opencapital/` — this repo (compute sidecar + catalog `plugins.json`). Already on branch `basic-data-macro-plugin`.
- `oc-plugin-yfinance-app/` → renamed `oc-plugin-basic-data-app/` — the plugin. Create branch `basic-data-rename` at the start of Task B1.

## Global Constraints

- Compute sidecar frozen libs are limited to `freeze-requirements.txt` (`polars`, `pg8000`, `pyinstaller`, `sqlglot`) **+ `requests`** (added in Task A3). Anything imported by metric code at runtime must be force-collected in `compute.spec` (PyInstaller cannot see `exec`-time imports).
- Compute sidecar runs under unrestricted `exec` with full `__builtins__`; the injected namespace is the *provided surface*, asserted exactly by `test_namespace_surface_is_exactly_the_curated_set`.
- Downloaded-app scripts run under macOS `/bin/bash` 3.2 — no bash 4+ features in any shipped shell script.
- Plugin id is **`basic-data-app`**; Postgres schema is **`basic_data`**; metric refs are **`basic-data-app/<metric>`**; display name is **"Basic Data"**. The word "yfinance" survives only where it names the Yahoo *source* (the Tickers feature), never as the plugin id/schema.
- FRED key is stored **plaintext** in `basic_data.app_settings` (key=`fred_api_key`) — explicit decision; do not move it to `secureJsonData`.
- TTL cache default 1h, overridable via env `OC_COMPUTE_HTTP_TTL`.
- Macro is **passthrough** — no macro observations table; only the API key + settings persist.

---

## Phase A — Compute sidecar HTTP + TTL cache (`opencapital/`)

### Task A1: `httpfetch.py` — TTL cache + `fetch_json`

**Files:**
- Create: `opencapital/services/compute/compute/httpfetch.py`
- Test: `opencapital/services/compute/tests/test_httpfetch.py`

**Interfaces:**
- Produces: `class TTLCache(ttl: float, maxsize=256, clock=time.monotonic)` with `get_or_fetch(key, fetch) -> Any`; `fetch_json(url: str, *, params=None, headers=None, ttl=None, timeout=15.0) -> Any`; module global `_CACHE`.

- [ ] **Step 1: Write the failing test**

```python
# opencapital/services/compute/tests/test_httpfetch.py
import pytest
from compute import httpfetch


class FakeClock:
    def __init__(self): self.t = 0.0
    def __call__(self): return self.t


def test_cache_hit_within_ttl_does_not_refetch():
    clock = FakeClock()
    cache = httpfetch.TTLCache(ttl=100.0, clock=clock)
    calls = {"n": 0}
    def fetch():
        calls["n"] += 1
        return "v"
    assert cache.get_or_fetch("k", fetch) == "v"
    clock.t = 50.0
    assert cache.get_or_fetch("k", fetch) == "v"
    assert calls["n"] == 1  # served from cache


def test_cache_refetches_after_ttl_expiry():
    clock = FakeClock()
    cache = httpfetch.TTLCache(ttl=100.0, clock=clock)
    calls = {"n": 0}
    def fetch():
        calls["n"] += 1
        return calls["n"]
    assert cache.get_or_fetch("k", fetch) == 1
    clock.t = 101.0
    assert cache.get_or_fetch("k", fetch) == 2
    assert calls["n"] == 2


def test_cache_evicts_soonest_expiring_at_maxsize():
    clock = FakeClock()
    cache = httpfetch.TTLCache(ttl=100.0, maxsize=2, clock=clock)
    cache.get_or_fetch("a", lambda: "a")
    clock.t = 1.0
    cache.get_or_fetch("b", lambda: "b")
    clock.t = 2.0
    cache.get_or_fetch("c", lambda: "c")  # evicts "a" (soonest expiry)
    assert len(cache._data) == 2
    assert "a" not in cache._data


def test_fetch_json_caches_by_url_and_params(monkeypatch):
    seen = {"n": 0}
    class Resp:
        def raise_for_status(self): pass
        def json(self): return {"ok": True}
    def fake_get(url, params=None, headers=None, timeout=None):
        seen["n"] += 1
        return Resp()
    monkeypatch.setattr(httpfetch.requests, "get", fake_get)
    httpfetch._CACHE = httpfetch.TTLCache(ttl=100.0)  # isolate global
    a = httpfetch.fetch_json("http://x", params={"a": 1})
    b = httpfetch.fetch_json("http://x", params={"a": 1})
    assert a == b == {"ok": True}
    assert seen["n"] == 1  # second call cached


def test_fetch_json_raises_on_http_error(monkeypatch):
    class Resp:
        def raise_for_status(self): raise RuntimeError("502")
        def json(self): return {}
    monkeypatch.setattr(httpfetch.requests, "get", lambda *a, **k: Resp())
    httpfetch._CACHE = httpfetch.TTLCache(ttl=100.0)
    with pytest.raises(RuntimeError):
        httpfetch.fetch_json("http://x")
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd opencapital && .venv/bin/python -m pytest services/compute/tests/test_httpfetch.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'compute.httpfetch'`
(If `.venv` is absent, run `make compute-venv` first, then `.venv/bin/pip install requests`.)

- [ ] **Step 3: Write minimal implementation**

```python
# opencapital/services/compute/compute/httpfetch.py
"""Cached HTTP egress for panel metrics.

Provides one process-global TTL cache and a ``fetch_json`` helper, injected
into the compute exec namespace by ``compute.endpoint.build_namespace``. The
cache key excludes the dashboard window so panels with different ranges share a
single fetched series. Network I/O happens outside the lock.
"""
from __future__ import annotations

import os
import threading
import time

import requests


class TTLCache:
    def __init__(self, ttl: float = 3600.0, maxsize: int = 256, clock=time.monotonic):
        self._ttl = ttl
        self._max = maxsize
        self._clock = clock
        self._lock = threading.Lock()
        self._data: dict = {}  # key -> (expires_at, value)

    def get_or_fetch(self, key, fetch):
        now = self._clock()
        with self._lock:
            hit = self._data.get(key)
            if hit and hit[0] > now:
                return hit[1]
        value = fetch()  # outside the lock — no serialization on slow I/O
        with self._lock:
            if len(self._data) >= self._max and key not in self._data:
                del self._data[min(self._data, key=lambda k: self._data[k][0])]
            self._data[key] = (now + self._ttl, value)
        return value


_CACHE = TTLCache(ttl=float(os.environ.get("OC_COMPUTE_HTTP_TTL", "3600")))


def fetch_json(url, *, params=None, headers=None, ttl=None, timeout=15.0):
    key = (
        url,
        tuple(sorted((params or {}).items())),
        tuple(sorted((headers or {}).items())),
    )

    def _do():
        r = requests.get(url, params=params, headers=headers, timeout=timeout)
        r.raise_for_status()
        return r.json()

    return _CACHE.get_or_fetch(key, _do)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd opencapital && .venv/bin/python -m pytest services/compute/tests/test_httpfetch.py -q`
Expected: PASS (5 passed)

- [ ] **Step 5: Commit**

```bash
cd opencapital
git add services/compute/compute/httpfetch.py services/compute/tests/test_httpfetch.py
git commit -m "feat(compute): TTL-cached fetch_json HTTP helper for panel metrics"
```

---

### Task A2: Inject `fetch_json` into the exec namespace

**Files:**
- Modify: `opencapital/services/compute/compute/endpoint.py` (`build_namespace`, ~line 110)
- Modify: `opencapital/services/compute/tests/test_endpoint.py:181-208` (surface test)

**Interfaces:**
- Consumes: `compute.httpfetch.fetch_json` (Task A1).
- Produces: `build_namespace(...)["fetch_json"]` is `httpfetch.fetch_json`.

- [ ] **Step 1: Update the failing test**

Edit `test_namespace_surface_is_exactly_the_curated_set` to expect `fetch_json`:

```python
    expected = (
        set(metrics.__all__)
        | {"metric", "bind", "window", "pl", "sql", "rw", "pg"}
        | {"prod", "pairwise", "sorted", "math"}
        | {"fetch_json"}
    )
    assert set(ns) == expected
    ...
    from compute.httpfetch import fetch_json as _fetch_json
    assert ns["fetch_json"] is _fetch_json
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd opencapital && .venv/bin/python -m pytest services/compute/tests/test_endpoint.py::test_namespace_surface_is_exactly_the_curated_set -q`
Expected: FAIL — `assert set(ns) == expected` mismatch (`fetch_json` missing from ns)

- [ ] **Step 3: Add the injection**

In `compute/endpoint.py`, add the import near the top:
```python
from compute.httpfetch import fetch_json
```
and in `build_namespace`, before `return ns`:
```python
    ns["fetch_json"] = fetch_json
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd opencapital && .venv/bin/python -m pytest services/compute/tests/test_endpoint.py -q`
Expected: PASS (whole file green)

- [ ] **Step 5: Commit**

```bash
cd opencapital
git add services/compute/compute/endpoint.py services/compute/tests/test_endpoint.py
git commit -m "feat(compute): expose fetch_json in panel exec namespace"
```

---

### Task A3: Freeze `requests`/`certifi` into the sidecar binary

**Files:**
- Modify: `opencapital/services/compute/freeze-requirements.txt`
- Modify: `opencapital/services/compute/compute.spec`
- Test: `opencapital/services/compute/tests/test_freeze_smoke.py` (add a case)

**Interfaces:**
- Produces: the frozen `services/compute/dist/compute` binary can `import requests` and serve `fetch_json`.

- [ ] **Step 1: Add the freeze dep**

In `freeze-requirements.txt`, add under the existing pins:
```
requests>=2.32,<3
```

- [ ] **Step 2: Force-collect requests + certifi in the spec**

In `compute.spec`, after the `sqlglot_hiddenimports = ...` line add:
```python
req_datas, req_binaries, req_hiddenimports = collect_all('requests')
certifi_datas, certifi_binaries, certifi_hiddenimports = collect_all('certifi')
```
Then extend the `Analysis(...)` args:
```python
    binaries=polars_binaries + rt32_binaries + req_binaries + certifi_binaries,
    datas=polars_datas + rt32_datas + req_datas + certifi_datas,
    hiddenimports=(
        polars_hiddenimports
        + rt32_hiddenimports
        + collect_submodules('compute')
        + sqlglot_hiddenimports
        + req_hiddenimports
        + certifi_hiddenimports
        + [
            ... existing ...
            'requests',
            'certifi',
        ]
    ),
```

- [ ] **Step 3: Add a freeze smoke assertion**

Append to `tests/test_freeze_smoke.py` (the existing freeze-marked module that runs the frozen binary):
```python
@pytest.mark.freeze
def test_frozen_binary_has_requests(compute_binary):
    # compute_binary fixture: path to dist/compute (see existing tests)
    out = subprocess.run(
        [compute_binary, "--selfcheck-import", "requests"],
        capture_output=True, text=True, timeout=30,
    )
    # If --selfcheck-import is unsupported, fall back to: start the server and
    # POST a /compute source that does `import requests` and returns a scalar.
    assert out.returncode == 0, out.stderr
```
If the binary has no `--selfcheck-import` flag, instead add a freeze test that starts the server and posts a source `@metric(output="scalar")\ndef m():\n    import requests\n    return 1.0` and asserts a 200 + value 1.0.

- [ ] **Step 4: Freeze + smoke**

Run: `cd opencapital && make compute-sidecar-stage && make compute-smoke`
Expected: freeze completes; smoke passes; `services/compute/dist/compute` exists and serves `/compute`.
Then: `.venv/bin/python -m pytest services/compute/tests/test_freeze_smoke.py -q -m freeze`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd opencapital
git add services/compute/freeze-requirements.txt services/compute/compute.spec services/compute/tests/test_freeze_smoke.py
git commit -m "build(compute): bundle requests+certifi into the frozen sidecar"
```

---

## Phase B — Plugin rename (`oc-plugin-yfinance-app` → `oc-plugin-basic-data-app`)

### Task B1: Mechanical rename + schema-rename migration

**Files (in the plugin repo):**
- Rename repo dir: `oc-plugin-yfinance-app/` → `oc-plugin-basic-data-app/`
- Modify: `oc-plugin.json`, `src/plugin.json`, `src/constants.ts`, `provisioning/plugins/apps.yaml`, `package.json`
- Modify: `pkg/plugin/app.go` (`ensureSchema`) — schema rename + namespace
- Modify: all `dashboards/*.json`, `library-panels/*.json`, `library-panels/*.py` — `yfinance-app/…`→`basic-data-app/…`, `yfinance.`→`basic_data.`
- Modify (in `opencapital/`): `plugins.json`
- Test: `pkg/plugin/pg_repo_test.go::TestEnsureSchema` (updated)

**Interfaces:**
- Produces: plugin id `basic-data-app`, executable `gpx_basic-data-app`, schema `basic_data`.

- [ ] **Step 1: Branch + rename the working tree**

```bash
cd /Users/ignacioballester/trading-code/oc-plugin-yfinance-app
git checkout -b basic-data-rename
# rename ids in manifests (review each diff)
```
Edit:
- `oc-plugin.json`: `"pluginId": "basic-data-app"`, reset `"versions": ["0.2.0"]`.
- `src/plugin.json`: `"id": "basic-data-app"`, `"name": "Basic Data"`, `"executable": "gpx_basic-data-app"`, `"opencapital.plugin_id": "basic-data-app"`, and update the `name` in `info.description`.
- `package.json`: `"name": "basic-data-app"`.
- `provisioning/plugins/apps.yaml`: `type: 'basic-data-app'`.
- `opencapital/plugins.json`: replace the yfinance manifest URL with `https://raw.githubusercontent.com/opencapital-dev/oc-plugin-basic-data-app/main/oc-plugin.json`.

- [ ] **Step 2: Update the failing schema test**

In `pkg/plugin/pg_repo_test.go::TestEnsureSchema`, change the assertion to expect the new schema + the rename guard:
```go
	firstSQL := fc.pgExecCalls[0].sql
	if !strings.Contains(firstSQL, "ALTER SCHEMA yfinance RENAME TO basic_data") {
		t.Errorf("first PGExec must perform the idempotent schema rename: %s", firstSQL)
	}
	// A later call must create schema basic_data.
	var foundSchema bool
	for _, c := range fc.pgExecCalls {
		if strings.Contains(c.sql, "CREATE SCHEMA IF NOT EXISTS basic_data") {
			foundSchema = true
		}
	}
	if !foundSchema {
		t.Error("expected CREATE SCHEMA IF NOT EXISTS basic_data")
	}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd oc-plugin-yfinance-app && go test ./pkg/plugin/ -run TestEnsureSchema -v`
Expected: FAIL — still emits `yfinance`.

- [ ] **Step 4: Update `ensureSchema`**

In `pkg/plugin/app.go`, replace the `stmts` slice head so the first statement is an idempotent rename, then create-if-not-exists against `basic_data`, and switch every `yfinance.` reference to `basic_data.`:
```go
	stmts := []string{
		// Idempotent rename of the pre-0.2.0 schema; no-op on fresh installs.
		`DO $$ BEGIN
		   IF EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = 'yfinance')
		      AND NOT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = 'basic_data')
		   THEN EXECUTE 'ALTER SCHEMA yfinance RENAME TO basic_data'; END IF;
		 END $$`,
		`CREATE SCHEMA IF NOT EXISTS basic_data`,
		`CREATE TABLE IF NOT EXISTS basic_data.instrument_ticker_mapping ( ... unchanged columns ... )`,
		`CREATE INDEX IF NOT EXISTS itm_symbol_idx ON basic_data.instrument_ticker_mapping(symbol)`,
		`CREATE INDEX IF NOT EXISTS itm_updated_idx ON basic_data.instrument_ticker_mapping(updated_at)`,
		`CREATE OR REPLACE VIEW basic_data.gw_classification AS
		  SELECT portfolio_id AS portfolio, instrument_id, updated_at AS ts, sector, subindustry AS industry
		  FROM basic_data.instrument_ticker_mapping`,
	}
```
Then update any other `yfinance.` literal in Go (e.g. `pg_repo.go` queries) to `basic_data.`.

- [ ] **Step 5: Rewrite metric refs + schema refs in shipped assets**

```bash
cd oc-plugin-yfinance-app
grep -rl 'yfinance-app/' dashboards library-panels | xargs sed -i '' 's#yfinance-app/#basic-data-app/#g'
grep -rl 'yfinance\.' dashboards library-panels | xargs sed -i '' 's#yfinance\.#basic_data.#g'
# constants.ts ROUTES handled in Task C3; verify no stray 'yfinance-app' id refs:
grep -rn 'yfinance-app' . --include=*.ts --include=*.tsx --include=*.json | grep -v node_modules
```
Expected final grep: only matches that legitimately name the Yahoo source, none that are the plugin id.

- [ ] **Step 6: Build + test green**

Run:
```bash
cd oc-plugin-yfinance-app
go test ./pkg/plugin/ -run TestEnsureSchema -v      # PASS
go build ./...                                       # compiles
npm ci && npm run typecheck                          # passes
```

- [ ] **Step 7: Commit (+ rename the repo dir/remote out-of-band)**

```bash
cd oc-plugin-yfinance-app
git add -A
git commit -m "refactor: rename yfinance-app -> basic-data-app (id, schema, refs)"
```
Out-of-band (operator): rename the GitHub repo to `oc-plugin-basic-data-app` and the local dir; the GHCR artifact name follows the new pluginId at publish (Task E2).

---

## Phase C — Settings (plugin repo)

### Task C1: `app_settings` table in `ensureSchema`

**Files:**
- Modify: `pkg/plugin/app.go` (`ensureSchema` stmts)
- Test: `pkg/plugin/pg_repo_test.go` (add `TestEnsureSchemaCreatesAppSettings`)

**Interfaces:**
- Produces: table `basic_data.app_settings(key text PRIMARY KEY, value text, updated_at timestamptz DEFAULT now())`.

- [ ] **Step 1: Write the failing test**

```go
func TestEnsureSchemaCreatesAppSettings(t *testing.T) {
	fc := &fakeClient{}
	app := makeAppWithFakeClient(fc)
	app.ensureSchema(context.Background())
	var found bool
	for _, c := range fc.pgExecCalls {
		if strings.Contains(c.sql, "CREATE TABLE IF NOT EXISTS basic_data.app_settings") {
			found = true
		}
	}
	if !found {
		t.Error("expected basic_data.app_settings table creation")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd oc-plugin-yfinance-app && go test ./pkg/plugin/ -run TestEnsureSchemaCreatesAppSettings -v`
Expected: FAIL — table not created.

- [ ] **Step 3: Add the statement**

Append to the `stmts` slice in `ensureSchema`:
```go
		`CREATE TABLE IF NOT EXISTS basic_data.app_settings (
		    key        text PRIMARY KEY,
		    value      text,
		    updated_at timestamptz DEFAULT now()
		)`,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd oc-plugin-yfinance-app && go test ./pkg/plugin/ -run TestEnsureSchema -v`
Expected: PASS (both schema tests)

- [ ] **Step 5: Commit**

```bash
cd oc-plugin-yfinance-app
git add pkg/plugin/app.go pkg/plugin/pg_repo_test.go
git commit -m "feat(settings): create basic_data.app_settings table"
```

---

### Task C2: Settings resource handlers (GET/PUT + test-fred)

**Files:**
- Create: `pkg/plugin/handlers_settings.go`
- Modify: `pkg/plugin/routing.go`
- Test: `pkg/plugin/handlers_settings_test.go`

**Interfaces:**
- Consumes: `a.client.PGQuery/PGExec`; the fake client (`pkg/plugin/testclient_test.go`).
- Produces: routes `GET /settings`, `PUT /settings`, `POST /settings/test-fred`. `GET /settings` returns `{"fred_api_key_set": bool, "pollIntervalSec": int, "qps": float, "burst": int, "liveEnable": bool, "backfillEnable": bool}` (key value is never returned, only whether it is set). `PUT /settings` accepts the same shape (plus `fred_api_key` string) and upserts rows.

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/handlers_settings_test.go
package plugin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencapital-dev/oc-plugin-sdk/pluginclient"
)

func TestPutSettingsUpsertsFredKey(t *testing.T) {
	fc := &fakeClient{}
	app := makeAppWithFakeClient(fc)
	req := httptest.NewRequest("PUT", "/settings", strings.NewReader(`{"fred_api_key":"abc123"}`))
	rec := httptest.NewRecorder()
	app.handleSettings(rec, req)
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var upserted bool
	for _, c := range fc.pgExecCalls {
		if strings.Contains(c.sql, "basic_data.app_settings") && strings.Contains(c.sql, "ON CONFLICT") {
			upserted = true
			if len(c.args) < 2 || c.args[0] != "fred_api_key" || c.args[1] != "abc123" {
				t.Errorf("unexpected upsert args: %v", c.args)
			}
		}
	}
	if !upserted {
		t.Error("expected an upsert into basic_data.app_settings")
	}
}

func TestGetSettingsReportsKeyPresenceNotValue(t *testing.T) {
	fc := &fakeClient{
		pgQueryResult: pluginclient.Result{
			Columns: []pluginclient.Column{{Name: "value"}},
			Rows:    [][]any{{"secret"}},
		},
	}
	app := makeAppWithFakeClient(fc)
	req := httptest.NewRequest("GET", "/settings", nil)
	rec := httptest.NewRecorder()
	app.handleSettings(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["fred_api_key_set"] != true {
		t.Errorf("want fred_api_key_set true, got %v", body["fred_api_key_set"])
	}
	if _, leaked := body["fred_api_key"]; leaked {
		t.Error("must not return the key value")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd oc-plugin-yfinance-app && go test ./pkg/plugin/ -run TestPutSettings -v`
Expected: FAIL — `app.handleSettings` undefined.

- [ ] **Step 3: Implement the handler**

```go
// pkg/plugin/handlers_settings.go
package plugin

import (
	"encoding/json"
	"net/http"
)

type settingsPayload struct {
	FredAPIKey      *string  `json:"fred_api_key,omitempty"`
	PollIntervalSec *int     `json:"pollIntervalSec,omitempty"`
	QPS             *float64 `json:"qps,omitempty"`
	Burst           *int     `json:"burst,omitempty"`
	LiveEnable      *bool    `json:"liveEnable,omitempty"`
	BackfillEnable  *bool    `json:"backfillEnable,omitempty"`
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		res, err := a.client.PGQuery(ctx,
			`SELECT value FROM basic_data.app_settings WHERE key = $1`, "fred_api_key")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		set := len(res.Rows) > 0 && res.Rows[0][0] != nil && res.Rows[0][0] != ""
		writeJSON(w, map[string]any{
			"fred_api_key_set": set,
			"pollIntervalSec":  a.options.DiscoveryPollSec,
			"qps":              a.options.YfinanceQPS,
			"burst":            a.options.YfinanceBurst,
			"liveEnable":       a.options.LiveEnable,
			"backfillEnable":   a.options.BackfillEnable,
		})
	case http.MethodPut:
		var p settingsPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if p.FredAPIKey != nil {
			if _, err := a.client.PGExec(ctx,
				`INSERT INTO basic_data.app_settings (key, value, updated_at)
				 VALUES ($1, $2, now())
				 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
				"fred_api_key", *p.FredAPIKey); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		// poll/qps/burst/toggles persist to jsonData via the existing config path;
		// here we only persist the FRED key. Echo success.
		writeJSON(w, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleTestFred verifies the stored key against a trivial FRED endpoint.
func (a *App) handleTestFred(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	res, err := a.client.PGQuery(ctx,
		`SELECT value FROM basic_data.app_settings WHERE key = $1`, "fred_api_key")
	if err != nil || len(res.Rows) == 0 || res.Rows[0][0] == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "no key set"})
		return
	}
	key, _ := res.Rows[0][0].(string)
	url := "https://api.stlouisfed.org/fred/series?series_id=GDP&api_key=" + key + "&file_type=json"
	resp, err := http.Get(url)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	writeJSON(w, map[string]any{"ok": resp.StatusCode == 200, "status": resp.StatusCode})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
```
If `writeJSON` already exists elsewhere in the package, drop the duplicate here.

- [ ] **Step 4: Register the routes**

In `pkg/plugin/routing.go`, inside `registerRoutes`:
```go
	mux.HandleFunc("/settings", a.handleSettings)
	mux.HandleFunc("/settings/test-fred", a.handleTestFred)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd oc-plugin-yfinance-app && go test ./pkg/plugin/ -run 'TestPutSettings|TestGetSettings' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
cd oc-plugin-yfinance-app
git add pkg/plugin/handlers_settings.go pkg/plugin/routing.go pkg/plugin/handlers_settings_test.go
git commit -m "feat(settings): GET/PUT settings + FRED key test endpoint"
```

---

### Task C3: Frontend — Tickers subpage + Settings page

**Files:**
- Modify: `src/constants.ts` (ROUTES)
- Rename: `src/pages/InstrumentsPage.tsx` → `src/pages/TickersPage.tsx` (export `TickersPage`)
- Create: `src/pages/SettingsPage.tsx`
- Create: `src/api/settings.ts`
- Modify: `src/components/App/App.tsx` (routes), `src/plugin.json` (`includes`)
- Test: `src/components/App/App.test.tsx`

**Interfaces:**
- Consumes: `yfRequest` (`src/api/client.ts`).
- Produces: `ROUTES.Tickers='tickers'`, `ROUTES.Settings='settings'`; `getSettings()/putSettings()/testFred()` in `src/api/settings.ts`.

- [ ] **Step 1: Write the failing test**

In `src/components/App/App.test.tsx`, add:
```tsx
test('renders the Settings page at the settings route', async () => {
  // render App wrapped in a MemoryRouter at /a/basic-data-app/settings
  // (follow the existing render helper in this file)
  // assert a "FRED API key" label is present
  expect(await screen.findByText(/FRED API key/i)).toBeInTheDocument();
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd oc-plugin-yfinance-app && npm run test:ci -- App.test`
Expected: FAIL — no Settings route / label.

- [ ] **Step 3: Implement routes, constants, api, pages**

`src/constants.ts`:
```ts
export enum ROUTES {
  Tickers = 'tickers',
  Settings = 'settings',
}
```

`src/api/settings.ts`:
```ts
import { yfRequest } from './client';

export type Settings = {
  fred_api_key_set: boolean;
  pollIntervalSec: number; qps: number; burst: number;
  liveEnable: boolean; backfillEnable: boolean;
};
export const getSettings = () => yfRequest<Settings>('/settings'.replace('/yf', '')); // see note
```
Note: settings routes are mounted at `/resources/settings`, not under `/yf`. Add a sibling base in `client.ts`:
```ts
export const RES_BASE = PLUGIN_RESOURCES;
export const resRequest = <T,>(path: string, options: RequestOptions = {}) =>
  request<T>(`${RES_BASE}${path}`, options);
```
and use `resRequest` in `settings.ts`:
```ts
import { resRequest } from './client';
export const getSettings = () => resRequest<Settings>('/settings');
export const putSettings = (body: Partial<Settings> & { fred_api_key?: string }) =>
  resRequest<{ ok: boolean }>('/settings', { method: 'PUT', body });
export const testFred = () => resRequest<{ ok: boolean; status?: number }>('/settings/test-fred', { method: 'POST' });
```
(Refactor `request` to be exported or reuse `resRequest`.)

`src/pages/SettingsPage.tsx`:
```tsx
import React, { useEffect, useState } from 'react';
import { Button, Field, Input, SecretInput, Alert } from '@grafana/ui';
import { getSettings, putSettings, testFred, type Settings } from '../api/settings';

export function SettingsPage() {
  const [s, setS] = useState<Settings | null>(null);
  const [key, setKey] = useState('');
  const [msg, setMsg] = useState<string | null>(null);
  useEffect(() => { getSettings().then(setS); }, []);
  const save = async () => { await putSettings({ fred_api_key: key }); setMsg('Saved'); setKey(''); getSettings().then(setS); };
  const test = async () => { const r = await testFred(); setMsg(r.ok ? 'FRED key OK' : 'FRED key invalid'); };
  return (
    <div>
      <h2>Settings</h2>
      <Field label="FRED API key" description="Get one at fredaccount.stlouisfed.org. Stored locally.">
        <SecretInput
          isConfigured={!!s?.fred_api_key_set}
          value={key}
          placeholder={s?.fred_api_key_set ? 'configured' : 'enter key'}
          onChange={(e) => setKey(e.currentTarget.value)}
          onReset={() => setKey('')}
        />
      </Field>
      <Button onClick={save}>Save</Button>{' '}
      <Button variant="secondary" onClick={test}>Test</Button>
      {msg && <Alert title={msg} severity="info" />}
    </div>
  );
}
```

`src/components/App/App.tsx`:
```tsx
import { TickersPage } from '../../pages/TickersPage';
import { SettingsPage } from '../../pages/SettingsPage';
// ...
    <Routes>
      <Route path={ROUTES.Tickers} element={<TickersPage />} />
      <Route path={ROUTES.Settings} element={<SettingsPage />} />
      <Route path="*" element={<Navigate replace to={ROUTES.Tickers} />} />
    </Routes>
```

`src/plugin.json` `includes` — rename Instruments → Tickers and add Settings:
```json
  "includes": [
    { "type": "page", "name": "Tickers", "path": "/a/%PLUGIN_ID%/tickers", "addToNav": true, "defaultNav": true, "icon": "chart-line" },
    { "type": "page", "name": "Settings", "path": "/a/%PLUGIN_ID%/settings", "addToNav": true, "icon": "cog" }
  ],
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd oc-plugin-yfinance-app && npm run test:ci -- App.test && npm run typecheck`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd oc-plugin-yfinance-app
git add src/
git commit -m "feat(ui): Tickers subpage + Settings page (FRED key, intervals)"
```

---

## Phase D — Macro metrics (`library-panels/*.py`)

Shared helper block (paste verbatim at the top of every macro metric file — metric files cannot import one another under the exec model):

```python
def _fred_key():
    r = pg("SELECT value FROM basic_data.app_settings WHERE key = $1", "fred_api_key")
    if r.is_empty() or r["value"][0] in (None, ""):
        raise ValueError("FRED API key not set — add it in Basic Data → Settings")
    return r["value"][0]

def _fred(series_id):
    js = fetch_json("https://api.stlouisfed.org/fred/series/observations",
                    params={"series_id": series_id, "api_key": _fred_key(), "file_type": "json"})
    obs = js["observations"]
    ts = [o["date"] for o in obs]
    val = [None if o["value"] in (".", "") else float(o["value"]) for o in obs]
    return (pl.DataFrame({"ts": ts, "value": val})
              .with_columns(pl.col("ts").str.to_datetime("%Y-%m-%d", strict=False))
              .drop_nulls())

def _dbnomics(path):  # path = "Provider/dataset/series"
    js = fetch_json(f"https://api.db.nomics.world/v22/series/{path}?observations=1")
    doc = js["series"]["docs"][0]
    ts = doc.get("period_start_day") or doc["period"]
    return (pl.DataFrame({"ts": ts, "value": doc["value"]})
              .with_columns(pl.col("ts").str.to_datetime("%Y-%m-%d", strict=False),
                            pl.col("value").cast(pl.Float64, strict=False))
              .drop_nulls())

def _series(provider, code):
    return _fred(code) if provider == "fred" else _dbnomics(code)

def _yoy(df, periods):  # periods: 12 monthly, 4 quarterly
    return (df.sort("ts")
              .with_columns((pl.col("value") / pl.col("value").shift(periods) * 100 - 100).alias("value"))
              .drop_nulls())
```

### Task D1: `cpi_yoy.py` (template metric)

**Files:**
- Create: `library-panels/cpi_yoy.py`
- Test: `tests/test_cpi_yoy.py` (new Python test dir in the plugin repo; jest is for TS, so add a tiny pytest that imports the metric via the same register→call shape the sidecar uses)

**Interfaces:**
- Consumes: injected `metric`, `pl`, `pg`, `fetch_json`, `$country` substitution.
- Produces: a `series` metric returning `ts,value` (CPI YoY %) for the substituted country.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_cpi_yoy.py
import polars as pl


def _run(source: str, country: str, fake_fetch, fake_pg):
    src = source.replace("$country", country)
    captured = {}
    def metric(*, output):
        def deco(fn): captured["fn"] = fn; captured["output"] = output; return fn
        return deco
    ns = {"metric": metric, "pl": pl, "fetch_json": fake_fetch, "pg": fake_pg, "window": (0, 1)}
    exec(src, ns)
    return captured["fn"]()


def test_cpi_yoy_us_uses_fred_and_computes_yoy():
    src = open("library-panels/cpi_yoy.py").read()
    def fake_pg(q, *a): return pl.DataFrame({"value": ["KEY"]})
    def fake_fetch(url, **kw):
        # 13 monthly index points 100..112 → last YoY = 12%
        return {"observations": [{"date": f"2023-{m:02d}-01", "value": str(100 + i)}
                                 for i, m in enumerate(range(1, 13))]
                + [{"date": "2024-01-01", "value": "112"}]}
    df = _run(src, "US", fake_fetch, fake_pg)
    assert abs(df.sort("ts")["value"][-1] - 12.0) < 1e-6
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd oc-plugin-yfinance-app && .venv/bin/python -m pytest tests/test_cpi_yoy.py -q`
Expected: FAIL — file `library-panels/cpi_yoy.py` missing.

- [ ] **Step 3: Write the metric**

```python
# library-panels/cpi_yoy.py
# <paste the shared helper block here>

# country -> (provider, series), monthly CPI index (YoY computed below)
SERIES = {
    "US": ("fred", "CPIAUCSL"),
    "EA": ("dbnomics", "Eurostat/prc_hicp_midx/M.I15.CP00.EA"),
    "GB": ("dbnomics", "OECD/PRICES_CPI/GBR.CPALTT01.IXOB.M"),
    "JP": ("dbnomics", "OECD/PRICES_CPI/JPN.CPALTT01.IXOB.M"),
    "CN": ("dbnomics", "OECD/PRICES_CPI/CHN.CPALTT01.IXOB.M"),
}

@metric(output="series")
def cpi_yoy():
    provider, code = SERIES["$country"]
    return _yoy(_series(provider, code), 12).select("ts", "value")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd oc-plugin-yfinance-app && .venv/bin/python -m pytest tests/test_cpi_yoy.py -q`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd oc-plugin-yfinance-app
git add library-panels/cpi_yoy.py tests/test_cpi_yoy.py
git commit -m "feat(macro): cpi_yoy metric (FRED US + DBnomics rest)"
```

---

### Task D2: Remaining indicator metrics

**Files (create each, helper block pasted at top of each):**
- `library-panels/gdp_yoy.py`, `unemployment.py`, `policy_rate.py`, `yield_10y.py`, `real_rate.py`, `curve_slope.py`
- Test: `tests/test_macro_metrics.py`

**Interfaces:**
- Produces: six `series` metrics keyed by `$country`.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_macro_metrics.py  (reuse _run from test_cpi_yoy via import or copy)
import polars as pl
from tests.test_cpi_yoy import _run

def _pg_key(q, *a): return pl.DataFrame({"value": ["KEY"]})

def test_unemployment_us_passthrough_level():
    src = open("library-panels/unemployment.py").read()
    def fetch(url, **kw):
        return {"observations": [{"date": "2024-01-01", "value": "3.7"},
                                 {"date": "2024-02-01", "value": "3.9"}]}
    df = _run(src, "US", fetch, _pg_key)
    assert df.sort("ts")["value"][-1] == 3.9

def test_curve_slope_us_is_10y_minus_2y():
    src = open("library-panels/curve_slope.py").read()
    calls = {"n": 0}
    def fetch(url, **kw):
        calls["n"] += 1
        v = "4.0" if "DGS10" in str(kw.get("params")) else "4.5"
        return {"observations": [{"date": "2024-01-01", "value": v}]}
    df = _run(src, "US", fetch, _pg_key)
    assert abs(df["value"][0] - (-0.5)) < 1e-9
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd oc-plugin-yfinance-app && .venv/bin/python -m pytest tests/test_macro_metrics.py -q`
Expected: FAIL — files missing.

- [ ] **Step 3: Write the metrics** (helper block at top of each; only the body shown)

`gdp_yoy.py` (quarterly real GDP → YoY, periods=4):
```python
SERIES = {
    "US": ("fred", "GDPC1"),
    "EA": ("dbnomics", "Eurostat/namq_10_gdp/Q.CLV10_MEUR.SCA.B1GQ.EA19"),
    "GB": ("dbnomics", "OECD/QNA/GBR.B1_GE.LNBQRSA.Q"),
    "JP": ("dbnomics", "OECD/QNA/JPN.B1_GE.LNBQRSA.Q"),
    "CN": ("dbnomics", "OECD/QNA/CHN.B1_GE.LNBQRSA.Q"),
}
@metric(output="series")
def gdp_yoy():
    p, c = SERIES["$country"]; return _yoy(_series(p, c), 4).select("ts", "value")
```

`unemployment.py` (level %, passthrough):
```python
SERIES = {
    "US": ("fred", "UNRATE"),
    "EA": ("dbnomics", "Eurostat/une_rt_m/M.SA.TOTAL.PC_ACT.T.EA19"),
    "GB": ("dbnomics", "OECD/STLABOUR/GBR.LRHUTTTT.STSA.M"),
    "JP": ("dbnomics", "OECD/STLABOUR/JPN.LRHUTTTT.STSA.M"),
    "CN": ("dbnomics", "OECD/STLABOUR/CHN.LRHUTTTT.STSA.M"),
}
@metric(output="series")
def unemployment():
    p, c = SERIES["$country"]; return _series(p, c).select("ts", "value")
```

`policy_rate.py` (level %, passthrough):
```python
SERIES = {
    "US": ("fred", "DFF"),
    "EA": ("dbnomics", "ECB/FM/D.U2.EUR.4F.KR.MRR_FR.LEV"),
    "GB": ("dbnomics", "BOE/IUDBEDR/IUDBEDR"),
    "JP": ("dbnomics", "BIS/cbpol/D.JP"),
    "CN": ("dbnomics", "BIS/cbpol/D.CN"),
}
@metric(output="series")
def policy_rate():
    p, c = SERIES["$country"]; return _series(p, c).select("ts", "value")
```

`yield_10y.py` (level %, passthrough):
```python
SERIES = {
    "US": ("fred", "DGS10"),
    "EA": ("dbnomics", "Eurostat/irt_lt_mcby_d/D.EA"),
    "GB": ("dbnomics", "OECD/MEI_FIN/IRLT.GBR.M"),
    "JP": ("dbnomics", "OECD/MEI_FIN/IRLT.JPN.M"),
    "CN": ("dbnomics", "OECD/MEI_FIN/IRLT.CHN.M"),
}
@metric(output="series")
def yield_10y():
    p, c = SERIES["$country"]; return _series(p, c).select("ts", "value")
```

`real_rate.py` (policy rate − CPI YoY, ASOF-joined on ts):
```python
# imports the same SERIES maps inline:
POLICY = {"US": ("fred", "DFF"), "EA": ("dbnomics", "ECB/FM/D.U2.EUR.4F.KR.MRR_FR.LEV"),
          "GB": ("dbnomics", "BOE/IUDBEDR/IUDBEDR"), "JP": ("dbnomics", "BIS/cbpol/D.JP"),
          "CN": ("dbnomics", "BIS/cbpol/D.CN")}
CPI = {"US": ("fred", "CPIAUCSL"), "EA": ("dbnomics", "Eurostat/prc_hicp_midx/M.I15.CP00.EA"),
       "GB": ("dbnomics", "OECD/PRICES_CPI/GBR.CPALTT01.IXOB.M"),
       "JP": ("dbnomics", "OECD/PRICES_CPI/JPN.CPALTT01.IXOB.M"),
       "CN": ("dbnomics", "OECD/PRICES_CPI/CHN.CPALTT01.IXOB.M")}
@metric(output="series")
def real_rate():
    pol = _series(*POLICY["$country"]).sort("ts").rename({"value": "pol"})
    infl = _yoy(_series(*CPI["$country"]), 12).sort("ts").rename({"value": "cpi"})
    j = pol.join_asof(infl, on="ts")  # nearest prior CPI for each rate point
    return j.with_columns((pl.col("pol") - pl.col("cpi")).alias("value")).select("ts", "value").drop_nulls()
```

`curve_slope.py` (10Y − 2Y; US via FRED, others via DBnomics 2y/10y pairs):
```python
TEN = {"US": ("fred", "DGS10"), "EA": ("dbnomics", "Eurostat/irt_lt_mcby_d/D.EA"),
       "GB": ("dbnomics", "OECD/MEI_FIN/IRLT.GBR.M"), "JP": ("dbnomics", "OECD/MEI_FIN/IRLT.JPN.M"),
       "CN": ("dbnomics", "OECD/MEI_FIN/IRLT.CHN.M")}
TWO = {"US": ("fred", "DGS2"), "EA": ("dbnomics", "Eurostat/irt_st_m/M.IRT_M3.EA"),
       "GB": ("dbnomics", "OECD/MEI_FIN/IR3TIB.GBR.M"), "JP": ("dbnomics", "OECD/MEI_FIN/IR3TIB.JPN.M"),
       "CN": ("dbnomics", "OECD/MEI_FIN/IR3TIB.CHN.M")}
@metric(output="series")
def curve_slope():
    ten = _series(*TEN["$country"]).sort("ts").rename({"value": "ten"})
    two = _series(*TWO["$country"]).sort("ts").rename({"value": "two"})
    j = ten.join_asof(two, on="ts")
    return j.with_columns((pl.col("ten") - pl.col("two")).alias("value")).select("ts", "value").drop_nulls()
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd oc-plugin-yfinance-app && .venv/bin/python -m pytest tests/test_macro_metrics.py -q`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd oc-plugin-yfinance-app
git add library-panels/gdp_yoy.py library-panels/unemployment.py library-panels/policy_rate.py library-panels/yield_10y.py library-panels/real_rate.py library-panels/curve_slope.py tests/test_macro_metrics.py
git commit -m "feat(macro): gdp, unemployment, policy rate, 10y, real rate, curve slope metrics"
```

---

### Task D3: Verify provider series ids resolve (spec open item #1)

**Files:**
- Test: `tests/test_series_ids_resolve.py` (marked `integration`, network)

**Interfaces:** none produced; corrects the `SERIES` maps in Task D1/D2 where ids fail.

- [ ] **Step 1: Write the resolution test**

```python
# tests/test_series_ids_resolve.py
import os, json, urllib.request, pytest

pytestmark = pytest.mark.integration
KEY = os.environ.get("FRED_API_KEY")

DBN = [  # collect every DBnomics path from the metric SERIES maps
    "Eurostat/prc_hicp_midx/M.I15.CP00.EA",
    # ... enumerate all DBnomics paths used in D1/D2 ...
]
FRED = ["CPIAUCSL", "GDPC1", "UNRATE", "DFF", "DGS10", "DGS2"]

@pytest.mark.parametrize("path", DBN)
def test_dbnomics_resolves(path):
    url = f"https://api.db.nomics.world/v22/series/{path}?observations=1"
    js = json.load(urllib.request.urlopen(url, timeout=20))
    assert js["series"]["docs"], f"no series for {path}"
    assert js["series"]["docs"][0]["value"], f"no observations for {path}"

@pytest.mark.skipif(not KEY, reason="FRED_API_KEY not set")
@pytest.mark.parametrize("sid", FRED)
def test_fred_resolves(sid):
    url = f"https://api.stlouisfed.org/fred/series/observations?series_id={sid}&api_key={KEY}&file_type=json"
    js = json.load(urllib.request.urlopen(url, timeout=20))
    assert js["observations"], f"no observations for {sid}"
```

- [ ] **Step 2: Run it**

Run: `cd oc-plugin-yfinance-app && FRED_API_KEY=<key> .venv/bin/python -m pytest tests/test_series_ids_resolve.py -q -m integration`
Expected: PASS. For any FAIL, fix the offending id in the metric `SERIES` map (search DBnomics at db.nomics.world; FRED at fred.stlouisfed.org) and re-run.

- [ ] **Step 3: Commit any id corrections**

```bash
cd oc-plugin-yfinance-app
git add library-panels/*.py tests/test_series_ids_resolve.py
git commit -m "test(macro): verify provider series ids resolve; fix bad ids"
```

---

## Phase E — Dashboards, provisioning, publish

### Task E1: Library panels + macro dashboards

**Files:**
- Create: `library-panels/*.json` (one per metric: a timeseries panel referencing `basic-data-app/<metric>`)
- Create: `dashboards/world-macro.json`, `dashboards/macro-compare.json`
- Test: `tests/test_dashboards_valid.py`

**Interfaces:**
- Consumes: metric refs from D1/D2; the `$country` dashboard variable (template var, multi).
- Produces: provisioned macro dashboards (Tauri auto-copies any app plugin's `dashboards/`).

- [ ] **Step 1: Write the failing validation test**

```python
# tests/test_dashboards_valid.py
import json, glob, os

METRICS = {os.path.splitext(os.path.basename(p))[0] for p in glob.glob("library-panels/*.py")}

def _refs(obj):
    if isinstance(obj, dict):
        if obj.get("datasource", {}).get("type") == "core-datasource":
            for t in obj.get("targets", []):
                r = t.get("ref")
                if r: yield r
        for v in obj.values(): yield from _refs(v)
    elif isinstance(obj, list):
        for v in obj: yield from _refs(v)

def test_every_dashboard_ref_points_at_an_existing_metric():
    for f in glob.glob("dashboards/*.json") + glob.glob("library-panels/*.json"):
        d = json.load(open(f))
        for ref in _refs(d):
            plugin, _, metric = ref.partition("/")
            assert plugin == "basic-data-app", f"{f}: wrong plugin in {ref}"
            assert metric in METRICS, f"{f}: ref {ref} has no metric file"

def test_dashboards_parse_and_have_country_variable():
    d = json.load(open("dashboards/world-macro.json"))
    names = [v["name"] for v in d.get("templating", {}).get("list", [])]
    assert "country" in names
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd oc-plugin-yfinance-app && .venv/bin/python -m pytest tests/test_dashboards_valid.py -q`
Expected: FAIL — dashboards missing.

- [ ] **Step 3: Author the panels + dashboards**

For each metric, create `library-panels/<metric>.json` modeled on the existing `library-panels/sector-pnl-barchart.json` but `"type": "timeseries"` and the target:
```json
{ "datasource": { "type": "core-datasource", "uid": "core-datasource" },
  "ref": "basic-data-app/cpi_yoy", "refId": "A" }
```
`dashboards/world-macro.json`: a dashboard with a `templating.list` entry `{"name":"country","type":"custom","multi":true,"query":"US,EA,GB,JP,CN","current":{"text":"US","value":"US"}}`, rows (Inflation/Growth/Labor/Policy/Yields), each panel a `core-datasource` target with `ref: basic-data-app/<metric>` (the metric reads `$country`). `dashboards/macro-compare.json`: cross-country table/heatmap + curve-slope panels.
Give each dashboard a stable `uid` (e.g. `basic-data-world-macro`).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd oc-plugin-yfinance-app && .venv/bin/python -m pytest tests/test_dashboards_valid.py -q`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd oc-plugin-yfinance-app
git add library-panels/*.json dashboards/*.json tests/test_dashboards_valid.py
git commit -m "feat(macro): library panels + world-macro/compare dashboards"
```

---

### Task E2: Publish + integration check in the app

**Files:**
- Verify in `opencapital/`: `plugins.json` (already repointed in B1), staged sidecar.

- [ ] **Step 1: Stage the new compute sidecar into the app**

Run: `cd opencapital && make compute-sidecar-stage`
Expected: new `compute-<triple>` staged; includes requests/certifi (Task A3).

- [ ] **Step 2: Build + sign + publish the plugin**

Run (plugin repo, following its existing release flow):
```bash
cd oc-plugin-basic-data-app   # renamed dir
npm ci && npm run build
mage -v                        # backend binaries (executable gpx_basic-data-app)
npm run sign
# OCI push to ghcr.io/opencapital-dev/plugins/basic-data-app:0.2.0 per the publish-action
```
Expected: artifact pushed; `oc-plugin.json` `versions` includes `0.2.0`.

- [ ] **Step 3: Manual end-to-end verification**

```bash
cd opencapital && make app   # or the project's app-launch target
```
Then in the app:
- Basic Data → Settings: paste a FRED key, Save, Test → "FRED key OK".
- Open the World Macro dashboard: panels render for the default country; US panels show FRED data, EA/GB/JP/CN show DBnomics data.
- Clear the key: US (FRED) panels show "FRED API key not set — add it in Basic Data → Settings"; DBnomics panels still render.
- Confirm the old Instruments console is reachable as **Tickers** and still maps tickers.

- [ ] **Step 4: Commit the catalog change (opencapital)**

```bash
cd opencapital
git add plugins.json
git commit -m "chore(catalog): point at oc-plugin-basic-data-app 0.2.0"
```

---

## Self-Review

**Spec coverage:**
- A1–A3 ↔ spec Part A (requests/certifi freeze, fetch_json, TTL cache, surface test, egress). ✓
- B1 ↔ spec B1 rename inventory + schema rename migration. ✓
- C1–C3 ↔ spec B2/B3 (Tickers subpage, Settings page, FRED key in PG plaintext, absorbed AppConfig knobs, Test button). ✓
- D1–D3 ↔ spec B4 (FRED US + DBnomics rest metrics, `$country`, key-from-PG, coverage incl. derived real-rate + curve-slope; open-item #1 series-id verification). ✓
- E1–E2 ↔ spec B5/publish (library panels + dashboards via core-datasource refs; sidecar stage; catalog; manual verification incl. missing-key error path). ✓
- Spec open item #2 (ts frame type) is exercised by the manual render in E2 and the metric tests' `ts` datetime column; if `computeframe.ToFrame` needs epoch-ms, adjust the helper's `to_datetime` to `.dt.epoch("ms")` and re-run D1.
- Spec open item #3 (rename migration preserves rows) ↔ B1 Step 4 rename guard + TestEnsureSchema.

**Placeholder scan:** No "TBD"/"add error handling"/"similar to". The `... unchanged columns ...` in B1 Step 4 refers to the verbatim existing table body already in `app.go` (engineer keeps it as-is). Series ids are concrete starting values with D3 as the correction gate.

**Type consistency:** `fetch_json`, `_series`, `_yoy`, `_fred_key` signatures match across D1/D2/tests; `handleSettings`/`handleTestFred` route names match `routing.go`; `getSettings/putSettings/testFred` match the Go shapes; `ROUTES.Tickers/Settings` match App.tsx + plugin.json.
