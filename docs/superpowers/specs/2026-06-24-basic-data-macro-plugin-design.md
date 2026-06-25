# Basic Data plugin — macro dashboards via compute passthrough

Date: 2026-06-24
Status: draft (pending user review → implementation plan)
Repos touched:
- `opencapital` (this repo) — compute sidecar change + catalog `plugins.json`
- `oc-plugin-yfinance-app` → renamed **`oc-plugin-basic-data-app`** — the plugin itself

## Problem

We want macroeconomic indicators (CPI, GDP, unemployment, policy rates, yields)
for the global majors (US, Eurozone, UK, Japan, China) presented as Grafana
dashboards inside the OpenCapital desktop app, sourced from FRED (US, direct API
with key) and DBnomics (rest, keyless). Rather than ship a separate plugin, we
**generalize the existing yfinance plugin into a "Basic Data" plugin** that
serves both market tickers and macro data.

## Goal

- Macro charts render as **provisioned Grafana dashboards** (same delivery path
  the ticker dashboards already use — Tauri auto-provisions any `type:app`
  plugin's `dashboards/` + `library-panels/`).
- Data is fetched **on demand (passthrough)** — no macro persistence — through
  the existing `core-datasource` + Python compute sidecar, by giving the sidecar
  an HTTP-fetch capability.
- The FRED API key is entered in a new in-plugin **Settings** page and stored in
  Postgres; the compute metric reads it from Postgres to make FRED calls.
- The current Instruments console becomes a **Tickers** subpage.

## Decisions (locked during brainstorming)

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | Fold macro into the yfinance plugin, **full-rename** it to `basic-data-app` | The plugin already has a Go backend + `secureJsonData` + an operator console; one coherent "data" plugin. Single-tenant makes the rename migration cheap. |
| 2 | Sources: **FRED (US, key) + DBnomics (EU/UK/JP/CN, keyless)** | FRED is deep/clean for US; DBnomics gives broad keyless global coverage. |
| 3 | **Passthrough** (no macro persistence) | User chose freshness/no-storage over offline/joins. |
| 4 | Realize passthrough through **`core-datasource` + compute metric** (Python `@metric` that fetches the API) — **not** a new datasource plugin and **not** SQL | A Python metric can fetch the API and return a frame; reuses the existing datasource. Avoids building/signing/provisioning a second datasource. |
| 5 | FRED key stored in **Postgres, plaintext** (`basic_data.app_settings`); the metric reads it via `pg()` | The compute sidecar already receives `POSTGRES_DSN`, so a metric can read the key with no env injection / no datasource secret plumbing. Plaintext accepted (free, read-only, rate-limited key; single-tenant local DB). |
| 6 | Add a **TTL cache** to the compute sidecar (hand-rolled, stdlib) | Many panels/refreshes hit the same series; cache softens latency + rate limits without persistence. |
| 7 | Macro presented as dashboards only — **no separate Macro management console**; add-your-own = inline Python in a panel | Curated catalog = shipped metrics + dashboards; keeps v1 lean. |

## Architecture

Two parts: a one-time **platform capability** added to the compute sidecar, and
the **plugin** that uses it.

```
Grafana macro panel
  └─ targets core-datasource, ref: basic-data-app/<metric>
       └─ core-datasource posts {source, window} to compute sidecar /compute
            └─ sidecar exec()s the metric Python:
                 key = pg("SELECT value FROM basic_data.app_settings WHERE key='fred_api_key'")
                 data = fetch_json("https://api.stlouisfed.org/fred/series/observations", params={...key...})   # TTL-cached
                 return pl.DataFrame(ts, value)   # output="series"
       └─ frame → Grafana time-series panel
```

No macro rows are stored. Only the API key + plugin settings persist (in PG).

---

## Part A — Compute sidecar HTTP + TTL cache (`opencapital/services/compute`)

### A1. Bundle an HTTP client into the frozen sidecar

The sidecar is a PyInstaller one-file binary built from `freeze-requirements.txt`
+ `compute.spec`. Author metric code runs under **unrestricted `exec`** (full
`__builtins__`), but PyInstaller's static analysis cannot see imports that only
appear inside runtime-`exec`'d source strings — so the HTTP client must be
**force-collected**.

- `freeze-requirements.txt`: add `requests` (pulls in **`certifi`**, which ships
  a CA bundle — this is what makes HTTPS work from a frozen macOS binary, where
  stdlib `urllib`+`ssl` otherwise fail with `SSLCertVerificationError` for lack
  of a system cert path).
- `compute.spec`: add `collect_all('requests')` + `collect_all('certifi')`
  (also pulls `urllib3` / `idna` / `charset_normalizer`).

`requests` over `httpx`: the `exec` model is synchronous and `ThreadingHTTPServer`
already gives per-panel concurrency, so async buys nothing; `requests` ships
`certifi` and has well-trodden PyInstaller hooks.

### A2. New module `compute/httpfetch.py`

A process-global, thread-safe TTL cache plus a `fetch_json` helper (stdlib only;
no cache library — `cachetools.TTLCache` still needs an external lock and adds a
freeze dependency for ~10 saved lines).

```python
import os, time, threading, requests

class TTLCache:
    def __init__(self, ttl=3600.0, maxsize=256):
        self._ttl, self._max = ttl, maxsize
        self._lock = threading.Lock()
        self._data = {}                      # key -> (expires_at_monotonic, value)

    def get_or_fetch(self, key, fetch):
        now = time.monotonic()
        with self._lock:
            hit = self._data.get(key)
            if hit and hit[0] > now:
                return hit[1]
        value = fetch()                      # network I/O OUTSIDE the lock
        with self._lock:
            if len(self._data) >= self._max:
                del self._data[min(self._data, key=lambda k: self._data[k][0])]
            self._data[key] = (now + self._ttl, value)
        return value

_CACHE = TTLCache(ttl=float(os.environ.get("OC_COMPUTE_HTTP_TTL", "3600")))

def fetch_json(url, *, params=None, headers=None, ttl=None, timeout=15.0):
    key = (url, tuple(sorted((params or {}).items())), tuple(sorted((headers or {}).items())))
    def _do():
        r = requests.get(url, params=params, headers=headers, timeout=timeout)
        r.raise_for_status()
        return r.json()
    return _CACHE.get_or_fetch(key, _do)
```

Design notes:
- Fetch happens **outside the lock** so a slow request does not serialize all
  panels. Tradeoff: two concurrent misses on the same key may both fetch
  (thundering herd) — acceptable for ~30 macro series; add per-key locks only if
  it bites (YAGNI).
- `time.monotonic()` (immune to wall-clock changes); lazy expiry on read; size
  cap with soonest-expiring eviction.
- Cache key intentionally **excludes the dashboard window** so panels with
  different time ranges share one fetched series.
- TTL override per call (`ttl=`) reserved; default 1h via `OC_COMPUTE_HTTP_TTL`.

### A3. Inject `fetch_json` into the exec namespace

In `compute/endpoint.py` `build_namespace`, add `ns["fetch_json"] = fetch_json`.
Update the curated-surface assertion test (the endpoint defines an exact
panel-facing surface). Do **not** expose raw `requests` — `fetch_json` is the
single controlled, cached egress point.

### A4. Egress posture

The sidecar previously spoke only loopback (pg8000 → RW/PG). It now also reaches
the public internet for macro APIs. This is a conscious posture change, scoped to
public macro data; documented here so it is not a surprise.

### A5. Tests (Part A)
- `TTLCache`: hit / miss / expiry with an injected fake clock; size-cap eviction.
- `fetch_json`: caches by url+params; window-independent; raises on non-2xx
  (monkeypatch `requests.get`).
- `build_namespace` surface includes `fetch_json` (exact-surface test updated).
- A representative metric using `fetch_json` + `pg()` returns a valid frame
  (mock both).

---

## Part B — `basic-data-app` (full rename of `yfinance-app`)

### B1. Rename inventory + migration

Single-tenant, so we control all installs; still a real migration, not a relabel.

1. **Repo:** `oc-plugin-yfinance-app` → `oc-plugin-basic-data-app`.
2. **`oc-plugin.json`:** `pluginId` `yfinance-app` → `basic-data-app`; restart
   `versions[]` at `0.2.0` (breaking).
3. **`src/plugin.json`:** `id` → `basic-data-app`, `name` → "Basic Data",
   `executable` → `gpx_basic-data-app`, `opencapital.plugin_id`, page `includes`.
4. **PG schema** `yfinance` → `basic_data`: `app.go` self-migration creates the
   new schema/tables; on upgrade, rename existing data
   (`ALTER SCHEMA yfinance RENAME TO basic_data`, idempotent guard) to preserve
   the user's ticker mappings.
5. **Dashboards + library-panels:** rewrite metric refs `yfinance-app/…` →
   `basic-data-app/…`, logical-view names, and panel SQL schema refs
   (`yfinance.gw_classification` → `basic_data.gw_classification`).
6. **`provisioning/plugins/apps.yaml`:** `type: 'yfinance-app'` →
   `'basic-data-app'`.
7. **Catalog:** `opencapital/plugins.json` → point at the new repo's
   `oc-plugin.json`.
8. **GHCR:** new artifact name under `opencapital-dev/plugins`.
9. **Sweep:** `grep -ri yfinance` across both `opencapital` and the plugin repo
   for any remaining hardcoded references (Tauri `DEFAULT_REQUIRED` does **not**
   list yfinance, so no required-plugin change). Note the product keeps the
   word "yfinance" only where it names the Yahoo source itself (the Tickers
   feature), not the plugin id.

### B2. Information architecture (pages)

- **Tickers** — today's Instruments console (ticker→instrument mapping, backfill
  / live status), moved to this subpage. Unchanged behavior.
- **Settings** — new (see B3).
- **Macro** — presented as provisioned **Grafana dashboards**, not a console
  page. A nav entry deep-links to the macro dashboard. No add/remove UI in v1;
  "add-your-own" = edit a panel's inline Python to point at another series.

`src/plugin.json` `includes`: a `page` for Tickers (`addToNav`, `defaultNav`),
a `page` for Settings, and a `dashboard`/link nav entry pointing at the macro
dashboard uid.

### B3. Settings page

- **FRED API key**: text field → saved to PG `basic_data.app_settings`
  (plaintext), with a "Get a key" link (`https://fredaccount.stlouisfed.org`)
  and a **Test** button (backend calls a trivial FRED endpoint with the key,
  reports ok/unauthorized). We do not mint keys — the user pastes their own.
- **Absorbs the old AppConfig knobs** the plugin already had: poll interval,
  qps, burst, live/backfill toggles (these belong to the Tickers/ingest side and
  move into this consolidated page).
- **Backend:** `app.go` self-migrates the settings table on startup; new resource
  handlers `GET/PUT settings` do `PGQuery`/`PGExec` against `basic_data.app_settings`.

```sql
CREATE TABLE IF NOT EXISTS basic_data.app_settings (
  key        text PRIMARY KEY,
  value      text,
  updated_at timestamptz DEFAULT now()
);
-- FRED key row: key = 'fred_api_key', value = '<plaintext>'
```

### B4. Macro metrics (`library-panels/*.py`)

Each macro chart is a shipped `@metric` source resolved by ref
`basic-data-app/<metric>` (the core-datasource resolver maps it to
`<installRoot>/basic-data-app/library-panels/<metric>.py`). The source fetches
live via `fetch_json` and returns a `pl.DataFrame` (`output="series"`).

- **US series → FRED (direct, key from PG):**
  ```python
  @metric(output="series")
  def cpi_yoy_us():
      key = pg("SELECT value FROM basic_data.app_settings WHERE key = $1", "fred_api_key")
      if key.is_empty() or key["value"][0] in (None, ""):
          raise ValueError("FRED API key not set — add it in Basic Data → Settings")
      js = fetch_json(
          "https://api.stlouisfed.org/fred/series/observations",
          params={"series_id": "CPIAUCSL", "api_key": key["value"][0], "file_type": "json"},
      )
      # parse js["observations"] → (date, value); value "." means missing → null
      ...
      return pl.DataFrame({"ts": ts_col, "value": val_col})
  ```
- **Non-US series → DBnomics (keyless):**
  ```python
  @metric(output="series")
  def cpi_yoy_ez():
      js = fetch_json("https://api.db.nomics.world/v22/series/Eurostat/prc_hicp_manr/M.RCH_A.CP00.EA?observations=1")
      # parse js["series"]["docs"][0]["period"][] + ["value"][]
      ...
  ```
- The small FRED-key boilerplate is duplicated across US metrics (metric files
  are self-contained; the exec model resolves one file per ref and does not
  import across metric files). Acceptable for v1.
- **Parameterization:** a `$country` dashboard variable selects which
  series/provider via a dict inside the metric, so one metric file serves all
  countries for an indicator (core-datasource substitutes `$country` into the
  source before exec).

**Coverage (curated v1 — global majors):** CPI YoY, GDP (YoY/level),
unemployment, policy rate, 10Y yield for US / Eurozone / UK / Japan / China,
plus two derived charts (real policy rate = policy rate − CPI YoY; yield-curve
slope = 10Y − 2Y). Exact provider series codes (FRED ids; DBnomics
provider/dataset/series triples) are validated during implementation — the plan
includes a verification step that each id resolves and returns observations.

### B5. Dashboards (`dashboards/*.json`)

- **`world-macro.json`** — rows by theme (Inflation, Growth, Labor, Policy
  rates, Yields), each a time-series panel overlaying the five economies via the
  `$country` (multi) variable. Panels target `{type: core-datasource, uid:
  core-datasource}` with `ref: basic-data-app/<metric>`.
- **`macro-compare.json`** — cross-country comparison (inflation table/heatmap,
  policy-rate compare, 10Y−2Y slope).
- Provisioned automatically by Tauri (`provision_dashboards` copies any
  `type:app` plugin's `dashboards/`); library panels likewise.

### B6. Data persistence

- **Macro: none** — passthrough; the only durable state is the API key + the
  consolidated settings in `basic_data.app_settings`.
- The Tickers side keeps its existing PG/RW tables unchanged (modulo the schema
  rename).

## Error handling

- Fetch failure / non-2xx → the metric raises → core-datasource returns a clean
  query error; the panel shows red with the message.
- Missing FRED key → FRED metrics raise "FRED API key not set — add it in Basic
  Data → Settings"; DBnomics metrics are unaffected.
- DBnomics/FRED malformed or empty series (e.g. a bad user-edited id) → parse
  guard raises a readable error.
- Cache serves last-good only within the TTL window; it is not a durable offline
  store (consistent with the passthrough choice).

## Security / privacy

- The FRED key is stored **plaintext** in the local Postgres control DB
  (explicit user decision). It is a free, read-only, rate-limited key on a
  single-tenant local database. Not placed in `secureJsonData` because the
  compute sidecar (a separate process) cannot read Grafana-encrypted plugin
  secrets, whereas it already has the PG DSN.
- Sidecar gains public-internet egress (Part A4).

## Testing

- **Part A:** unit tests per A5.
- **Part B:**
  - A macro metric unit test: mock `fetch_json` + `pg()`, assert the parsed
    frame shape (ts/value, missing-value handling).
  - Dashboards + library-panel JSON validate (schema / parse) and reference
    only existing metric refs.
  - Settings handlers: PG read/write round-trip via the test client; Test-key
    handler maps FRED 200/401 correctly.
  - Rename: schema-migration test (yfinance → basic_data rename preserves rows).
  - Manual: launch the app, open the macro dashboard, confirm panels render;
    set/clear the FRED key and confirm US panels toggle between data and the
    "key not set" error.

## Publish / release

- `services/compute`: rebuild the frozen sidecar with the new deps
  (`make compute-sidecar-stage` per the spec error message), ship in the app
  bundle.
- `basic-data-app`: frontend build + backend mage binaries + sign + OCI push to
  GHCR; bump `versions[]`; repoint `opencapital/plugins.json`.
- App bundle ships `dashboards/` + `library-panels/` as today.

## Out of scope (v1)

- Macro persistence / revision vintages / offline (passthrough by choice).
- A Macro management console (add/remove/search series in UI).
- SQL-joining macro against portfolios (would require persistence).
- ALFRED vintage history; intraday macro.
- Encrypting the FRED key.

## Open items for the implementation plan

1. Validate exact provider series ids (FRED ids; DBnomics triples) for the
   curated coverage — each must resolve and return observations.
2. Confirm the `ts` column type `computeframe.ToFrame` expects for a time-series
   output (epoch-ms vs datetime) and match it in the metrics.
3. Confirm the schema-rename migration path on an existing install preserves the
   user's ticker mappings (rename vs copy).
