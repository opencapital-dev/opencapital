# Serviceless Local Data Plane — Implementation Plan (Spec A)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the write-gateway, read-gateway, and control-plane services and Kafka from the local-first desktop app; have the compute sidecar and plugins talk Postgres (OLTP) and RisingWave (streaming) directly over pgwire; fold the plugin catalog/marketplace/install brain into Tauri.

**Architecture:** Six in-repo subsystems (A1–A6), each its own plan section below. RisingWave stays a single default schema reached over pgwire (per-plugin schema isolation is Spec B, out of scope). Postgres stays as the OLTP store for `portfolios`, bridged to RW by CDC. Tauri absorbs control-plane's catalog logic in Rust (`reqwest` already present).

**Tech Stack:** Rust (Tauri shell, `reqwest`), Python 3 + polars + **pg8000** (compute sidecar; pure-Python pgwire for PyInstaller), RisingWave + Postgres SQL, Go (services being deleted), Make.

## Global Constraints

- **Bash 3.2 compatibility** — bundled scripts run under macOS `/bin/bash` 3.2; no `mapfile`, no associative arrays, no `${var^^}`.
- **Compute freeze-friendliness** — the compute sidecar may import only stdlib + polars + pg8000; no `requests`/`httpx`/`psycopg` (C-extension or heavy deps break the one-file PyInstaller bundle).
- **No org/JWT on the data path** — after this spec, no component verifies a JWT or scopes by `org_id` for DB access; loopback trust only.
- **`portfolio_id` is the sole scope key** — everywhere `org_id` is removed, `portfolio_id` remains the scoping column.
- **Idempotent schema apply** — `apply.sh` and the host reconciler must remain safe to re-run (every DDL `IF NOT EXISTS` / tracked in `_schema_migrations`).
- **Single user, loopback only** — all services bind `127.0.0.1`.

## Cross-cutting: companion repos & landing order

Plugin/SDK code lives in **separate repos** (`oc-plugin-sdk`, `oc-plugin-core-app`, `oc-plugin-core-datasource`, `oc-plugin-yfinance-app`) and gets its own plan cycles. This plan defines the in-repo work + the contracts those repos target. `[LOCKSTEP]` marks a task that is only safe once a named companion change has landed.

**Recommended execution order** (not strict 1→6; A2/A4 and A3/A6 are cutover pairs):

1. **A1** — compute gains a pgwire `sql()` path. Additive, non-breaking. Safe first.
2. **A5** — catalog ported into Tauri (Rust), behind existing UI calls. Additive. Safe early.
3. *(companion)* SDK + plugins gain direct pgwire read/write; metric sources rewritten to `sql()`; core plugin writes `portfolios` to Postgres directly.
4. **A4 + A2** (cutover 1) — delete both gateways; drop `org_id` + Kafka sources. Do together: write-gateway stamps `org_id`, read-gateway's compiler *requires* it, so neither the column nor the services can go halfway.
5. **A6 + A3** (cutover 2) — delete control-plane + auth; move surviving `control_db` DDL into the host reconciler. Do together: control-plane owns the migrations until the reconciler takes them.

---

# Plan A1 — Compute sidecar: read via RisingWave pgwire

**Goal:** Replace the compute sidecar's read-gateway HTTP client with a direct RisingWave pgwire client, and replace the `bind(selector)` authoring contract with a `sql(query, *params)` primitive that returns a polars frame.

**Files:**
- Create: `services/compute/compute/rwclient.py` (pgwire connection + query → frame)
- Modify: `services/compute/compute/endpoint.py` (namespace assembly; drop prefetch/bind, inject `sql`)
- Modify: `services/compute/compute/server.py:91-112` (env: `RISINGWAVE_DSN` replaces `READ_GATEWAY_URL`)
- Modify: `services/compute/compute/contract.py:62-71` (remove `Binding`/`bind`; keep `Window`, `metric`, `Registry`)
- Delete: `services/compute/compute/gateway.py` (after `_frame_from` is moved to `rwclient.py`)
- Modify: `services/compute/freeze-requirements.txt` (add `pg8000`)
- Modify: `services/compute/compute.spec` (hiddenimports: `pg8000`)
- Modify: `opencapital-app/src-tauri/src/compute.rs:26-35,42-54,102-112` (SpawnSpec + env)
- Test: `services/compute/tests/test_rwclient.py` (new), `services/compute/tests/test_endpoint.py` (rewrite stub)

**Interfaces:**
- Consumes: nothing from other A-plans.
- Produces:
  - `rwclient.connect(dsn: str) -> Connection` — a pg8000 connection.
  - `rwclient.query(conn, sql: str, params: tuple) -> pl.DataFrame` — runs SQL, returns a frame with the same dtype/null handling `_frame_from` had (`ts` forced `Int64`).
  - `endpoint.run_compute(body: dict, dsn: str) -> dict` — new signature (`dsn` replaces `base_url`).
  - Metric exec namespace exposes `sql(query: str, *params) -> pl.DataFrame` and `window` (unchanged `Window`), `metric`, `pl`, curated stdlib. **[contract]** for plugin metric authors.
  - `/compute` request body: `{"source", "window": {"from","to"}, "prefetched"?}` — the `jwt` field is removed. **[contract]**

- [ ] **Step 1: Add pg8000 to the freeze inputs**

Modify `services/compute/freeze-requirements.txt`:
```
# Deps to freeze the compute sidecar into a one-file binary (Tauri externalBin).
polars>=1,<2
pg8000>=1.30,<2
pyinstaller>=6,<7
```

- [ ] **Step 2: Install pg8000 into the venv and confirm import**

Run: `.venv/bin/pip install 'pg8000>=1.30,<2' && .venv/bin/python -c "import pg8000.native; print('ok')"`
Expected: `ok`

- [ ] **Step 3: Write the failing test for rwclient.query frame shaping**

Create `services/compute/tests/test_rwclient.py`:
```python
import polars as pl
from compute.rwclient import _frame_from


def test_frame_forces_ts_int64_and_preserves_nulls():
    df = _frame_from(["ts", "nav"], [[1000, 1.5], [2000, None]])
    assert df.schema["ts"] == pl.Int64
    assert df["nav"].to_list() == [1.5, None]


def test_empty_rows_yield_typed_zero_height_frame():
    df = _frame_from(["ts", "nav"], [])
    assert df.height == 0
    assert df.schema["ts"] == pl.Int64
```

- [ ] **Step 4: Run it to verify it fails**

Run: `.venv/bin/python -m pytest services/compute/tests/test_rwclient.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'compute.rwclient'`

- [ ] **Step 5: Create rwclient.py (move `_frame_from`, add pgwire connect/query)**

Create `services/compute/compute/rwclient.py`:
```python
"""Direct RisingWave pgwire client — run SQL, return a polars frame.

pg8000 is pure-Python (PyInstaller-freeze-friendly). RisingWave speaks the
Postgres wire protocol, so the simple-query path works unchanged. Replaces the
read-gateway HTTP hop (compute/gateway.py).
"""
from __future__ import annotations

import logging
from urllib.parse import urlparse

import pg8000.native
import polars as pl

log = logging.getLogger("compute.rwclient")


def connect(dsn: str) -> pg8000.native.Connection:
    """Open a pg8000 connection from a postgres:// DSN (loopback, trust auth)."""
    u = urlparse(dsn)
    return pg8000.native.Connection(
        user=u.username or "root",
        password=u.password or None,
        host=u.hostname or "127.0.0.1",
        port=u.port or 4566,
        database=(u.path or "/dev").lstrip("/") or "dev",
    )


def query(conn: pg8000.native.Connection, sql: str, params: tuple = ()) -> pl.DataFrame:
    """Run *sql* with positional *params* ($1,$2,…) and return a polars frame."""
    rows = conn.run(sql, *params)
    columns = [c["name"] for c in conn.columns]
    return _frame_from(columns, rows)


def _frame_from(columns: list[str], rows: list[list]) -> pl.DataFrame:
    """Build a polars frame from column names + row arrays in column order.

    Each column built non-strictly so mixed int/float coerces to a common dtype;
    `ts` forced to Int64; empty rows yield a typed zero-height frame.
    """
    series = []
    for i, name in enumerate(columns):
        vals = [row[i] for row in rows]
        dtype = pl.Int64 if name == "ts" else None
        series.append(pl.Series(name, vals, dtype=dtype, strict=False))
    return pl.DataFrame(series)
```

- [ ] **Step 6: Run the frame test to verify it passes**

Run: `.venv/bin/python -m pytest services/compute/tests/test_rwclient.py -q`
Expected: PASS (2 passed)

- [ ] **Step 7: Rewrite the contract — drop `Binding`/`bind`, keep `Window`/`metric`/`Registry`**

In `services/compute/compute/contract.py`: remove the `Binding` dataclass (lines 62-71), the `_parse_selector` helper (lines 74-94), the `bind` method on `Contract` (and `bindings`/`raw_selectors` fields on `Registry`). Keep `Window`, `Registry.entrypoint`/`output`, `Contract.metric`, `make_contract`, `require_complete`. The metric no longer pre-declares inputs; it pulls data via `sql()` at call time.

- [ ] **Step 8: Inject `sql` into the exec namespace; drop prefetch**

In `services/compute/compute/endpoint.py`, change `build_namespace` and `run_compute`:
```python
def build_namespace(contract, window, sql_fn) -> dict:
    ns: dict = {name: getattr(metrics, name) for name in metrics.__all__}
    ns.update(_CURATED_STDLIB)
    ns["metric"] = contract.metric
    ns["window"] = window
    ns["pl"] = pl
    ns["sql"] = sql_fn
    return ns


def run_compute(body: dict, dsn: str) -> dict:
    source, window = _parse_body(body)          # jwt + prefetched no longer read
    conn = rwclient.connect(dsn)
    try:
        def sql_fn(query: str, *params):
            return rwclient.query(conn, query, tuple(params))
        contract = make_contract()
        ns = build_namespace(contract, window, sql_fn)
        exec(source, ns)
        contract.registry.require_complete()
        result = contract.registry.entrypoint()   # entrypoint calls sql() itself
        return _to_frame(contract.registry.output, result)
    finally:
        conn.close()
```
Update `_parse_body` to return `(source, Window(t0, t1))` only (remove `jwt`, `prefetched`). Update the import: `from compute import rwclient` (drop `from compute.gateway import …`).

- [ ] **Step 9: Point the server at the DSN env**

In `services/compute/compute/server.py`, change `ComputeServer.__init__`/`from_env` to carry `dsn` instead of `gateway_url`:
```python
_DEFAULT_DSN = "postgres://root@127.0.0.1:4566/dev?sslmode=disable"

# in __init__: self.dsn = dsn
# in from_env:
dsn = os.environ.get("RISINGWAVE_DSN", _DEFAULT_DSN)
return cls(host=host, port=port, dsn=dsn)
```
In the `/compute` handler, call `run_compute(body, self.server.dsn)`. Remove the `GatewayError` import + handling branch (keep `ComputeError`/`ContractError`).

- [ ] **Step 10: Rewrite the endpoint test to stub pgwire, not HTTP**

Replace `services/compute/tests/test_endpoint.py`'s HTTP gateway stub with a monkeypatched `rwclient.query`. Example test:
```python
import polars as pl
from compute import endpoint, rwclient


def test_run_compute_calls_sql_and_frames_result(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: object())
    monkeypatch.setattr(
        rwclient, "query",
        lambda conn, q, p: pl.DataFrame({"ts": [1, 2], "nav": [10.0, 11.0]}),
    )
    source = (
        "@metric(output='series')\n"
        "def m():\n"
        "    df = sql('SELECT ts, nav FROM nav WHERE portfolio = $1', 'p1')\n"
        "    return df\n"
    )
    out = endpoint.run_compute({"source": source, "window": {"from": 0, "to": 9}}, "dsn")
    assert out["columns"] == ["ts", "nav"]
    assert out["rows"] == [[1, 10.0], [2, 11.0]]
```

- [ ] **Step 11: Run the compute unit suite**

Run: `.venv/bin/python -m pytest services/compute/tests/test_rwclient.py services/compute/tests/test_endpoint.py services/compute/tests/test_contract.py -q`
Expected: PASS (all green; no references to `gateway`/`Binding`/`jwt`)

- [ ] **Step 12: Delete gateway.py and grep for stragglers**

Run: `rm services/compute/compute/gateway.py && grep -rn "gateway\|Binding\|READ_GATEWAY\|\.jwt" services/compute/compute services/compute/tests`
Expected: no matches (besides unrelated comments). Fix any remaining import.

- [ ] **Step 13: Wire the DSN from Tauri**

In `opencapital-app/src-tauri/src/compute.rs`: rename `SpawnSpec.read_gateway_url` → `risingwave_dsn` (line 31), set it from `cfg.risingwave_dsn` (line ~52, AppConfig already exposes `risingwave_dsn` per config.rs), and change the env in `spawn_compute` (line 108) from `.env("READ_GATEWAY_URL", …)` to `.env("RISINGWAVE_DSN", &spec.risingwave_dsn)`.

- [ ] **Step 14: Add the pg8000 hidden import to the freeze spec**

In `services/compute/compute.spec`, add `"pg8000"` to `hiddenimports`. Then freeze + smoke:
Run: `cd services/compute && ../../.venv/bin/pyinstaller compute.spec >/dev/null && ./dist/compute & sleep 8 && curl -s localhost:8790/health; kill %1`
Expected: `ok` (the frozen binary boots and serves health; pg8000 bundled).

- [ ] **Step 15: Commit**

```bash
git add services/compute opencapital-app/src-tauri/src/compute.rs
git commit -m "feat(compute): read RisingWave over pgwire (pg8000), sql() metric primitive"
```

---

# Plan A2 — RisingWave schema: drop org_id, kill Kafka sources

**Goal:** Remove `org_id` from all 16 RW schema files (`portfolio_id` becomes the sole scope), promote the connector-less landing tables to canonical, and delete the Kafka source variant + dead `apply.sh` logic.

`[LOCKSTEP]` This plan drops the `org_id` column from the landing tables. The write-gateway INSERTs `org_id`, and the read-gateway compiler *refuses* a view without `org_id` (`compiler.go:55-62`). **Execute A2 together with A4** (gateway deletion) and after the companion plugins write `org_id`-free rows. Do not land A2 while either gateway still runs.

**Files (all under `dataplane/risingwave/`):**
- Delete: `schemas/01-sources/` (the Kafka variant: `portfolio_events_log.sql`, `data_log.sql`)
- Rename: `schemas/01-sources-local/*` → `schemas/01-sources/*` (connector-less becomes canonical)
- Modify (drop org_id): `schemas/01-sources/{portfolio_events_log,data_log}.sql`, `schemas/02-control-plane/01-portfolios-cdc.sql`, `schemas/03-unifying-views/{prices,option_marks}.sql`, `schemas/03b-instruments/instruments.sql`, `schemas/04-fx/fx_rates.sql`, `schemas/04b-events/events.sql`, `schemas/05-fold/fold_per_event.sql`, `schemas/06-metrics/{cash_per_tick,instrument_per_tick,instrument_per_event,portfolio_per_tick,closures_per_event,cycles_per_event}.sql`, `schemas/08-ingestor-discovery/{00-instruments_used,01-fx_pairs_used,02-ohlcv_coverage,03-data_coverage,03-instruments_catalog}.sql`, `schemas/10-entities/{nav,flows,portfolio,closures,cycles,events,instrument,cash,price}.sql`
- Modify: `apply.sh` (drop PACKAGING switch, topics-seed wait, Kafka secret substitutions)
- Create: `scripts/rw_golden_nav.sh` (seed + NAV assertion harness — no golden test exists today)

**Interfaces:**
- Produces: landing-table column contract **without** `org_id` — `portfolio_events_log(source_id, event_type, portfolio_id, instrument_id, business_ts, ingest_ts, source, plugin_id, trace_id, payload, rw_key PK)` and `data_log(source_namespace, source_id, portfolio_id, observed_at, ingest_ts, source, plugin_id, trace_id, payload, rw_key PK)`. **[contract]** for ingestor plugins.

- [ ] **Step 1: Build a golden NAV harness (there is no RW regression test today)**

Create `scripts/rw_golden_nav.sh` — seeds a tiny fixed portfolio + one price into the landing tables over pgwire and prints NAV from `portfolio_per_tick`, so the same script run before and after the edit proves NAV is unchanged:
```bash
#!/usr/bin/env bash
# Seed one portfolio_events_log trade + one data_log price, print NAV.
# Usage: RW_HOST=localhost RW_PORT=4566 bash scripts/rw_golden_nav.sh
set -euo pipefail
PSQL=(psql -h "${RW_HOST:-localhost}" -p "${RW_PORT:-4566}" -U "${RW_USER:-root}" -d "${RW_DB:-dev}" -v ON_ERROR_STOP=1 --no-psqlrc -tA)
"${PSQL[@]}" <<'SQL'
INSERT INTO portfolio_events_log (source_id,event_type,portfolio_id,instrument_id,business_ts,ingest_ts,source,plugin_id,trace_id,payload,rw_key)
VALUES ('s1','TRADE','PF1','AAPL','2024-01-01T00:00:00Z',NOW(),'golden','core','t','{"quantity":10,"price":100,"currency":"USD","direction":"BUY","base_currency":"USD"}','PF1|core|s1');
INSERT INTO data_log (source_namespace,source_id,portfolio_id,observed_at,ingest_ts,source,plugin_id,trace_id,payload,rw_key)
VALUES ('prices.ohlcv','AAPL','PF1','2024-01-02T00:00:00Z',NOW(),'golden','yf','t','{"close":110,"currency":"USD"}','PF1|yf|prices.ohlcv|AAPL|2024-01-02');
SQL
sleep 2
"${PSQL[@]}" -c "SELECT scope_id, ROUND(nav_base::numeric,2) FROM portfolio_per_tick WHERE scope_id='PF1' ORDER BY event_ts DESC LIMIT 1;"
```
Run it once against the current (org_id-present) schema to capture the baseline NAV. Note: requires a portfolios row for `PF1`; seed via the existing portfolios path or add an INSERT into the RW `portfolios` table for the harness.

Run: `chmod +x scripts/rw_golden_nav.sh && RW_HOST=localhost bash scripts/rw_golden_nav.sh`
Expected: prints `PF1|1100.00` (10 × 110). Record this value.

- [ ] **Step 2: Promote connector-less sources to canonical**

```bash
git rm dataplane/risingwave/schemas/01-sources/portfolio_events_log.sql \
       dataplane/risingwave/schemas/01-sources/data_log.sql
git mv dataplane/risingwave/schemas/01-sources-local/portfolio_events_log.sql \
       dataplane/risingwave/schemas/01-sources/portfolio_events_log.sql
git mv dataplane/risingwave/schemas/01-sources-local/data_log.sql \
       dataplane/risingwave/schemas/01-sources/data_log.sql
rmdir dataplane/risingwave/schemas/01-sources-local
```

- [ ] **Step 3: Drop `org_id` from the two landing tables**

In `schemas/01-sources/portfolio_events_log.sql` remove the `org_id VARCHAR,` column (was line 19 of the local variant). In `schemas/01-sources/data_log.sql` remove the `org_id VARCHAR,` column (was line 15). Update the header comments referencing `org_id|plugin_id|source_id` to `plugin_id|source_id`.

- [ ] **Step 4: Drop `org_id` from the CDC portfolios table**

In `schemas/02-control-plane/01-portfolios-cdc.sql`: remove the `org_id VARCHAR,` column (line 9) and change `PRIMARY KEY (org_id, portfolio_id)` → `PRIMARY KEY (portfolio_id)` (line 15). (The Postgres-side `portfolios` PK change is A3.)

- [ ] **Step 5: Drop `org_id` from the fold partition**

In `schemas/05-fold/fold_per_event.sql`: remove `e.org_id,` from the SELECT (line 30) and change `PARTITION BY e.org_id, e.portfolio_id` → `PARTITION BY e.portfolio_id` (line 37).

- [ ] **Step 6: Drop `org_id` from unifying views, events, fx, instruments**

Remove every `org_id` projection / `USING (org_id, …)` / `ON x.org_id = y.org_id` clause in: `03-unifying-views/prices.sql` (lines 26,40), `03-unifying-views/option_marks.sql` (line 21), `04b-events/events.sql` (lines 40,51,62,81→`USING (portfolio_id)`,83,87), `04-fx/fx_rates.sql` (lines 37,44→`USING (portfolio_id)`,49,56,62,72), `03b-instruments/instruments.sql` (lines 14,21→`GROUP BY portfolio_id, instrument_id`). Each `USING (org_id, portfolio_id)` becomes `USING (portfolio_id)`; each `ON a.org_id=b.org_id AND …` drops the org_id conjunct.

- [ ] **Step 7: Drop `org_id` from the metrics MVs**

In `06-metrics/{portfolio_per_tick,instrument_per_tick,cash_per_tick,instrument_per_event,closures_per_event,cycles_per_event}.sql`: remove every `org_id` from SELECT projections, `GROUP BY org_id, …`, `USING (org_id, …)`, `ON …org_id…`, and the `MAX(source_id) … GROUP BY org_id,portfolio_id,business_ts` collapse subqueries (→ `GROUP BY portfolio_id, business_ts`, `USING (portfolio_id, business_ts, source_id)`). Use the per-file line lists from the grounding report; these are the densest files (portfolio_per_tick ~30 refs).

- [ ] **Step 8: Drop `org_id` from discovery MVs and entity views**

In `08-ingestor-discovery/{00-instruments_used,01-fx_pairs_used,02-ohlcv_coverage,03-data_coverage,03-instruments_catalog}.sql` and all nine `10-entities/*.sql`: remove the leading `org_id,` projection (and the `GROUP BY`/`JOIN ... org_id` clauses in `00-instruments_used`, `01-fx_pairs_used`, `03-instruments_catalog`).

- [ ] **Step 9: Strip Kafka logic from apply.sh**

In `dataplane/risingwave/apply.sh`: delete the `PACKAGING`/`SOURCES_EXCLUDE` block (lines 54-64) and the filter loop that drops a source flavour (lines 233-242) — there is only one sources dir now. Delete the `topics-seed` wait (lines 96-111) and the `load_substitution "@@SR_RW_PASSWORD@@"` / `"@@RW_KAFKA_SASL_PASSWORD@@"` calls (lines 146-147). Keep the `@@CDC_PG_HOST@@` substitution. (Bash 3.2: keep the existing array-append style.)

- [ ] **Step 10: Remove the PACKAGING env from the host apply call**

In `opencapital-app/src-tauri/src/dataplane.rs` `apply_rw_schema` (lines 305-342): remove `.env("PACKAGING", "local")` (line 325). Keep `CDC_PG_HOST`, `RW_HOST/PORT/USER/DB`.

- [ ] **Step 11: Re-apply schema on a fresh RW and verify NAV unchanged**

Run: `CDC_PG_HOST=127.0.0.1 RW_HOST=localhost bash dataplane/risingwave/apply.sh && RW_HOST=localhost bash scripts/rw_golden_nav.sh`
Expected: schema applies with no error; NAV prints `PF1|1100.00` — identical to Step 1. If different, the org_id edit broke a join; diff against the failing MV.

- [ ] **Step 12: Commit**

```bash
git add dataplane/risingwave opencapital-app/src-tauri/src/dataplane.rs scripts/rw_golden_nav.sh
git commit -m "refactor(rw): drop org_id, make connector-less landing tables canonical, remove Kafka sources"
```

---

# Plan A3 — Postgres: shed identity tables, host reconciler owns surviving DDL

**Goal:** Keep `control_db.portfolios` (re-keyed on `portfolio_id`) and the `rw_v6_pub` CDC publication; delete the identity tables; move the surviving DDL out of the deleted control-plane migrator into the host boot reconciler.

`[LOCKSTEP]` Execute together with **A6** — control-plane runs these migrations until it is deleted. Until the core plugin writes portfolios to Postgres directly (companion), keep the portfolios table populated by whatever still writes it.

**Files:**
- Create: `dataplane/postgres/init/02-portfolios.sql` (portfolios table + publication + grants, idempotent)
- Modify: `opencapital-app/src-tauri/src/dataplane.rs:248-265` (`bootstrap_control_db` runs the new file)
- Delete (with A6): `services/control-plane/internal/migrate/migrations/*` and `migrate/migrate.go`
- Reference: `dataplane/postgres/init/01-schema.sql` (roles — stays)

**Interfaces:**
- Produces: `control_db.portfolios(portfolio_id UUID PK, base_currency VARCHAR, attributes JSONB, updated_at TIMESTAMPTZ, updated_by VARCHAR)` + `PUBLICATION rw_v6_pub FOR TABLE portfolios` + `GRANT SELECT ON portfolios TO rw_replicator`. **[contract]** the core plugin writes this table; CDC reads it.

- [ ] **Step 1: Write the portfolios + publication init SQL (org_id dropped)**

Create `dataplane/postgres/init/02-portfolios.sql`:
```sql
-- Canonical portfolios reference table (OLTP). Single-user/local: no org_id.
-- Written by the core plugin over pgwire; CDC-published to RisingWave.
CREATE TABLE IF NOT EXISTS portfolios (
    portfolio_id  UUID         PRIMARY KEY,
    base_currency VARCHAR      NOT NULL,
    attributes    JSONB        NOT NULL DEFAULT '{}'::jsonb,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_by    VARCHAR      NOT NULL DEFAULT 'core'
);
GRANT USAGE ON SCHEMA public TO rw_replicator;
GRANT SELECT ON portfolios TO rw_replicator;
-- Publication for the RisingWave pg_cdc source (slot rw_v6_slot).
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = 'rw_v6_pub') THEN
    CREATE PUBLICATION rw_v6_pub FOR TABLE portfolios;
  END IF;
END $$;
```

- [ ] **Step 2: Run the surviving DDL from the host reconciler**

In `opencapital-app/src-tauri/src/dataplane.rs` `bootstrap_control_db` (lines 248-265): after it runs `01-schema.sql`, add a step that runs `02-portfolios.sql` via the same `psql -U postgres -d control_db -f …` invocation. Make it run every boot (idempotent), not just on first DB creation, so the table/publication self-heal.

- [ ] **Step 3: Verify portfolios + publication exist after a clean boot**

Run: `psql -h 127.0.0.1 -U postgres -d control_db -tAc "SELECT count(*) FROM information_schema.tables WHERE table_name='portfolios'; SELECT pubname FROM pg_publication;"`
Expected: `1` and `rw_v6_pub`.

- [ ] **Step 4: Verify CDC still flows portfolios → RW**

Run: `psql -h 127.0.0.1 -U postgres -d control_db -c "INSERT INTO portfolios(portfolio_id,base_currency,updated_by) VALUES (gen_random_uuid(),'USD','test') ON CONFLICT DO NOTHING;" && sleep 2 && psql -h localhost -p 4566 -U root -d dev -tAc "SELECT count(*) FROM portfolios;"`
Expected: RW `portfolios` count ≥ 1 (CDC replicated the row).

- [ ] **Step 5: Remove the identity-table migrations (deferred with A6)**

When A6 deletes control-plane, `git rm -r services/control-plane`. The identity migrations (`0001-0004,0014,0015,0026`) and `migrate.go` go with it. Confirm nothing else imports `internal/migrate`.

Run: `grep -rn "internal/migrate" services lib opencapital-app 2>/dev/null`
Expected: no matches outside `services/control-plane`.

- [ ] **Step 6: Commit**

```bash
git add dataplane/postgres/init/02-portfolios.sql opencapital-app/src-tauri/src/dataplane.rs
git commit -m "feat(postgres): host reconciler owns portfolios + rw_v6_pub; drop org_id"
```

---

# Plan A4 — Delete the gateways; rewire Tauri, Makefile, instance-bootstrap

**Goal:** Remove `services/gateway` and `services/read-gateway` and every reference to them in the shell, build, and provisioning.

`[LOCKSTEP]` Only after A1 (compute reads RW directly) **and** the companion datasource plugin reads RW pgwire directly **and** the companion write plugins INSERT to the landing tables directly. Pair with A2.

**Files:**
- Delete: `services/gateway/`, `services/read-gateway/`, `lib/datakey/` (sole consumer was the gateway)
- Modify: `opencapital-app/src-tauri/src/dataplane.rs` (constants 43-44, DSN 49, steps 6-7 lines 131-151, spawn fns 212-229)
- Modify: `opencapital-app/src-tauri/src/lib.rs:39-77` (port + path kill lists)
- Modify: `opencapital-app/src-tauri/src/proxy.rs:45-46` (`gw_child`, `rg_child`)
- Modify: `opencapital-app/src-tauri/src/config.rs:24-28,93-94` (gateway_url/read_gateway_url)
- Modify: `opencapital-app/src-tauri/src/grafana.rs:327-328` (env passthrough)
- Modify: `Makefile:10` (`GO_SVCS`)
- Modify: `lib/instance-bootstrap/bootstrap.go` (Config fields 56-62, jsonData 485/491/543, validation 614-615)

**Interfaces:**
- Consumes: A1's `RISINGWAVE_DSN` compute wiring; A2's org_id-free landing tables.
- Produces: instance-bootstrap renders `risingwaveHost`/`risingwavePort` (+ Postgres coords) to plugins, no `gatewayUrl`/`readGatewayUrl`. **[contract]**

- [ ] **Step 1: Remove gateway + read-gateway from the boot sequence**

In `dataplane.rs`: delete step 6 (lines 131-140) and step 7 (lines 142-151), the `spawn_gateway` (212-221) and `spawn_read_gateway` (223-229) functions, the `GW_PORT`/`RG_PORT` constants (43-44), and `GATEWAY_REPLICA_DSN` (49).

- [ ] **Step 2: Remove the child slots and kill-list entries**

In `proxy.rs`: delete `gw_child` and `rg_child` (lines 45-46). In `lib.rs`: remove `GW_PORT`/`RG_PORT` from the port-kill loop (lines 58-62 → keep only `CP_PORT` for now; A6 removes that too) and remove `"gateway"`, `"read-gateway"` from the exe-path kill list (line 72).

- [ ] **Step 3: Drop the gateway URLs from config + grafana env**

In `config.rs`: delete `gateway_url`/`read_gateway_url` fields (24-28) and their `pick(...)` loads (93-94). In `grafana.rs`: delete the `.env("PLUGIN_GATEWAY_URL", …)` / `.env("PLUGIN_READ_GATEWAY_URL", …)` lines (327-328).

- [ ] **Step 4: Update instance-bootstrap rendering**

In `lib/instance-bootstrap/bootstrap.go`: delete `PluginGatewayURL`/`PluginReadGatewayURL` Config fields (56-62) and their validation (614-615); remove `"gatewayUrl"`/`"readGatewayUrl"` from the app jsonData map (485, 491) and `"gatewayUrl"` from the datasource map (543). Add Postgres coordinates fields + render `"postgresHost"`, `"postgresPort"`, `"controlDb"` alongside `"risingwaveHost"`/`"risingwavePort"`.

- [ ] **Step 5: Shrink GO_SVCS and delete the service trees**

In `Makefile` line 10: `GO_SVCS := control-plane` (read-gateway/gateway removed; control-plane stays until A6). Then:
```bash
git rm -r services/gateway services/read-gateway lib/datakey
```

- [ ] **Step 6: Verify no dangling references**

Run: `grep -rn "read-gateway\|read_gateway\|readGatewayUrl\|gw_child\|rg_child\|GATEWAY_REPLICA\|spawn_gateway\|lib/datakey\|PluginGatewayURL" opencapital-app Makefile lib services 2>/dev/null`
Expected: no matches.

- [ ] **Step 7: Build the shell + bootstrap**

Run: `cd opencapital-app/src-tauri && cargo build 2>&1 | tail -5; cd - && (cd lib/instance-bootstrap && go build ./...)`
Expected: both compile clean.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "refactor: delete write/read gateways; rewire shell, Makefile, instance-bootstrap to direct pgwire"
```

---

# Plan A5 — Port the plugin catalog into Tauri (Rust)

**Goal:** Reproduce control-plane's catalog/federation/version/artifact logic in the Tauri shell so the marketplace works with no control-plane HTTP roundtrip. Additive — the existing UI commands in `kinde.rs` switch from remote HTTP to in-process calls.

**Files:**
- Create: `opencapital-app/src-tauri/src/catalog/mod.rs` (public API), `catalog/manifest.rs` (manifest + list fetch/cache), `catalog/registry.rs` (semver, version status, artifact resolve, GHCR token dance), `catalog/sources.rs` (official ∪ user-added, verified badge)
- Modify: `opencapital-app/src-tauri/src/kinde.rs` (catalog/source/version commands call `catalog::*` instead of `*_req()` HTTP)
- Modify: `opencapital-app/src-tauri/src/config.rs` (persist user-added sources locally — JSON file, mirroring selection/pin I/O)
- Modify: `opencapital-app/src-tauri/Cargo.toml` (none expected — `reqwest` with `json`+`rustls` already present)
- Test: `opencapital-app/src-tauri/src/catalog/registry.rs` `#[cfg(test)]` (semver sort, verified badge, blob URL)

**Interfaces (reproduce the Go surface, see grounding for exact signatures):**
- Produces:
  - `catalog::list(sources) -> Vec<Plugin>` (official Verified=true ∪ user-added Verified=false, deduped by URL)
  - `catalog::versions_with_status(id) -> Vec<VersionStatus>` (validated vs preview, semver desc)
  - `catalog::resolve_artifact(id, version, platform) -> Artifact { download_url, sha256, size_bytes }`
  - blob URL format `{publicURL}/v2/{namespace}/{id}/blobs/sha256:{digest}` (preserve exactly)

- [ ] **Step 1: Write the failing semver/badge tests**

Create `opencapital-app/src-tauri/src/catalog/registry.rs` with a `#[cfg(test)]` module first:
```rust
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn sorts_semver_desc_mixed_prefix() {
        let got = sort_semver_desc(&["0.1.2".into(), "v0.1.10".into(), "0.1.3".into()]);
        assert_eq!(got, vec!["v0.1.10", "0.1.3", "0.1.2"]);
    }
    #[test]
    fn blob_url_shape() {
        let u = blob_url("https://ghcr.io", "acme/oc-plugins", "core-app",
                         "sha256:abcd");
        assert_eq!(u, "https://ghcr.io/v2/acme/oc-plugins/core-app/blobs/sha256:abcd");
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd opencapital-app/src-tauri && cargo test catalog:: 2>&1 | tail -5`
Expected: FAIL — module/functions not found.

- [ ] **Step 3: Implement semver + blob_url + the structs**

Implement `Plugin`, `Artifact`, `VersionStatus`, `SourceInfo`, `normalize_semver`, `sort_semver_desc`, `blob_url` in `registry.rs` (mirror `semver.go` normalization: bare `0.1.3` ≡ `v0.1.3`, return original form). Add `mod catalog;` to `lib.rs`.

- [ ] **Step 4: Run to verify pass**

Run: `cd opencapital-app/src-tauri && cargo test catalog:: 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Port manifest + list fetch (reqwest, TTL cache)**

In `catalog/manifest.rs`: `PluginManifest`/`RegistrySpec` structs (serde), `fetch_manifest(url) -> PluginManifest` and `fetch_list(url) -> Vec<String>` using the shared `reqwest::Client`, with a 60s in-memory TTL cache and serve-stale-on-refresh-failure (match `manifest.go`). Validate URLs are http(s).

- [ ] **Step 6: Port sources union + verified badge**

In `catalog/sources.rs`: read the official list (Verified=true) ∪ user-added URLs from the local JSON store (Verified=false), dedupe by URL (official wins), skip unreachable manifests without failing the whole catalog (match `sources.go` + `TestProviderUnionAndVerified`).

- [ ] **Step 7: Port artifact resolution (GHCR anonymous token dance)**

In `catalog/registry.rs`: `resolve_artifact(id, version, platform)` fetches the OCI image manifest (anonymous `ghcr.io/token` noop-token flow that `reqwest` can do with a `WWW-Authenticate` retry), matches the platform layer annotation, returns `Artifact{ download_url = blob_url(...), sha256, size_bytes }`.

- [ ] **Step 8: Repoint the Tauri commands**

In `kinde.rs`: `marketplace_catalog`, `plugin_versions`, `list_sources`, `add_source`, `remove_source` call `catalog::*` in-process instead of `catalog_req()`/`list_sources()`/etc. HTTP to control-plane. Install/uninstall + selection/pin file I/O stay as-is for now (they target control-plane until A6; the provisioning handoff is finished in A6).

- [ ] **Step 9: Verify catalog parity against the live registry**

Run: `cd opencapital-app/src-tauri && cargo test catalog:: -- --nocapture 2>&1 | tail -10`
Expected: unit tests pass. Manually: launch the app, open the marketplace, confirm core-app/core-datasource/yfinance appear with versions + verified badges identical to the control-plane-served list.

- [ ] **Step 10: Commit**

```bash
git add opencapital-app/src-tauri
git commit -m "feat(tauri): port plugin catalog/federation/artifact resolution to Rust"
```

---

# Plan A6 — Delete control-plane + auth/identity; fold instance-bootstrap into Tauri

**Goal:** Remove `services/control-plane` and all auth/identity, and **fold the
`instance-bootstrap` reconciler into the Tauri Rust shell** — deleting the last Go
sidecar. Tauri resolves the install set (A5 catalog), then downloads → verifies →
extracts → renders Grafana provisioning in-process, no handoff file.

`[LOCKSTEP]` Only after A5 (catalog in Tauri) **and** the companion core plugin writes portfolios to Postgres directly (control-plane's portfolio handlers can then go). Pair with A3.

**Files:**
- Delete: `services/control-plane/` (whole tree), `lib/jwks/`, **`lib/instance-bootstrap/` (whole tree — folded into Tauri)**
- Create: `opencapital-app/src-tauri/src/reconcile/mod.rs` (+ `download.rs`, `provision.rs`, `dashboards.rs`) — the Rust reconciler
- Modify: `opencapital-app/src-tauri/src/dataplane.rs` (remove `CP_PORT` 42, step 3 control-plane spawn 105-115, `spawn_control_plane`, `CONTROL_PLANE_URL` 51, `LOCAL_TOKEN` 56)
- Modify: `opencapital-app/src-tauri/src/lib.rs` (remove `CP_PORT` kill + `"control-plane"` path kill; `mod reconcile;`)
- Modify: `opencapital-app/src-tauri/src/proxy.rs` (`cp_child`)
- Modify: `opencapital-app/src-tauri/src/kinde.rs` (`instance_token`/mint → removed; reconcile calls the in-process reconciler, no local file)
- Modify: `opencapital-app/src-tauri/src/grafana.rs` (call the in-process reconciler before spawning `grafana-server`, not the sidecar command)
- Modify: `opencapital-app/src-tauri/Cargo.toml` (add `sha2`, `tar`, `flate2`, `serde_yaml` if absent)
- Modify: `Makefile` (`GO_SVCS :=` empty; delete the `go-sidecars` target entirely — no Go sidecars remain)

**Interfaces:**
- Consumes: A5 `catalog::resolve_artifact` + `catalog::list`.
- Produces: `reconcile::run(cfg, install_set) -> Result<(), String>` — downloads + verifies (sha256) + extracts each artifact (idempotent via `.artifact-sha256`), symlinks into the plugins dir, renders Grafana provisioning YAML (datasource + app jsonData with `risingwaveHost`/`risingwavePort` + Postgres coords, no gateway URLs). **[contract]** replaces the entire `lib/instance-bootstrap` binary.

- [ ] **Step 1: Audit `lib/instance-bootstrap` — port vs delete**

Read `bootstrap.go`, `reconcile.go`, `provision_dashboards.go`, `library_panels.go`, `metric_deps.go` (~4k LOC). Classify each unit: **port** (download/verify/extract/symlink, datasource+app provisioning YAML, dashboard provider YAML, library-panel install) vs **delete** (`metric_deps.go` — it resolves the old read-gateway/DSL `query_entities` surface, which the `sql()` model removes; confirm nothing downstream needs it, then drop). Write the port/delete decision per file into the report before coding.

- [ ] **Step 2: Implement the Rust reconciler (TDD the pure parts)**

Create `src-tauri/src/reconcile/`. Port the classified-port units: `download.rs` (reqwest GET → `sha2` verify against the artifact sha256 → `tar`/`flate2` extract → symlink, idempotent via the `.artifact-sha256` marker), `provision.rs` (render datasource + app provisioning YAML via `serde_yaml`, jsonData = `risingwaveHost`/`risingwavePort` + `postgresHost`/`postgresPort`/`controlDb`, NO `gatewayUrl`/`readGatewayUrl`), `dashboards.rs` (dashboard provider YAML + library-panel install). Unit-test the pure logic (sha256 verify pass/fail, idempotency marker skip, rendered-YAML shape) offline; gate any network-touching test `#[ignore]`. `grafana.rs` calls `reconcile::run(...)` in-process before spawning `grafana-server`, replacing `instance_bootstrap_command` + the sidecar child.

- [ ] **Step 3: Remove control-plane from the boot sequence**

In `dataplane.rs`: delete step 3 (control-plane spawn, lines 105-115), `spawn_control_plane`, the `CP_PORT` constant (42), `CONTROL_PLANE_URL` (51), `LOCAL_TOKEN` (56). In `proxy.rs`: delete `cp_child`. In `lib.rs`: remove `CP_PORT` from the kill loop and `"control-plane"` from the path-kill list.

- [ ] **Step 4: Delete the service tree + jwks lib + instance-bootstrap; drop the Go-sidecar build**

```bash
git rm -r services/control-plane lib/jwks lib/instance-bootstrap
```
In `Makefile`: `GO_SVCS` is empty and no Go sidecars remain — delete the `go-sidecars` target and its references in `dataplane-stage`/`app-stage`; trim `go-build`/`go-test` to no-ops or remove. Remove `CONTROL_DB_DSN` usage if control-plane was its only reader (the host reconciler uses `psql`, not the DSN constant). Remove the `config.rs` `lib/instance-bootstrap/go.mod` repo-root marker lookup.

- [ ] **Step 5: Verify no dangling references + clean build**

Run: `grep -rn "control-plane\|CONTROL_PLANE_URL\|cp_child\|CP_PORT\|lib/jwks\|lib/instance-bootstrap\|instance_token\|/jwt/mint\|bootstrap_token\|instance_bootstrap_command" opencapital-app Makefile lib 2>/dev/null`
Expected: no matches (besides the postgres role name `control_plane` in init SQL, which stays).

Run: `cd opencapital-app/src-tauri && cargo build 2>&1 | tail -5`
Expected: compiles clean. No Go build remains for the shell side.

- [ ] **Step 6: End-to-end smoke — all services + the last Go sidecar gone, app works**

Run the app (`make app` or the dev launch). Verify: Postgres + RisingWave + compute spawn; no gateway/read-gateway/control-plane/instance-bootstrap processes; marketplace lists plugins (Tauri catalog); plugins install + provision via the in-process reconciler; a portfolio created in the UI appears in `control_db.portfolios` and flows to RW; a panel renders a metric (compute → RW pgwire).
Expected: NAV/metrics render; `ps aux | grep -E "gateway|control-plane|instance-bootstrap"` shows nothing.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "refactor: delete control-plane + auth/identity; Tauri owns catalog + provisioning handoff"
```

---

## Self-Review

**Spec coverage:**
- Remove 3 services → A1 (compute repoint), A4 (gateways), A6 (control-plane). ✓
- Kill Kafka/SR → A2 (sources + apply.sh). ✓
- Drop org_id/JWT → A2 (RW), A3 (PG portfolios), A1 (compute jwt), A6 (auth). ✓
- Direct pgwire read/write → A1 (compute), A4 ([contract] plugins). ✓
- Two-store + CDC → A3 (portfolios + publication, host reconciler). ✓
- Catalog → Tauri → A5. ✓
- instance-bootstrap folded into Tauri (last Go sidecar deleted) → A6 Steps 1-2,4. ✓
- Delete plugin_installs + identity tables → A3 Step 5 / A6. ✓

**Placeholder scan:** No "TBD"/"handle errors"/"similar to". Mechanical edits cite verbatim current code + line numbers from grounding; the one absent test harness (RW golden) is built in A2 Step 1.

**Type consistency:** `_frame_from(columns, rows)` identical in A1 Steps 3/5/10. `Artifact{download_url, sha256, size_bytes}` consistent A5↔A6. `rw_v6_pub`/`portfolio_id`-PK consistent A2 Step 4 ↔ A3 Step 1. `RISINGWAVE_DSN` consistent A1 Step 9/13 ↔ A4.

**Known honest gaps (not placeholders):**
- A2/A4 and A3/A6 are cutover pairs — between landing them, the data path is broken until the companion repos repoint. Marked `[LOCKSTEP]`; sequence with the SDK/plugin plans.
- A1 changes the metric authoring contract (`bind`→`sql`); existing plugin metric sources must be rewritten in their repos before A4 cutover.
- RisingWave RBAC for per-plugin schemas is Spec B; this plan keeps a single default schema + single trusted connection.
