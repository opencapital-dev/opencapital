# Panel Direct-SQL Bindings — Design

**Date:** 2026-06-21
**Status:** Approved (brainstorming) — ready for implementation planning

## Goal

Replace the dead read-gateway **DSL** in the library panels with **direct SQL**
bindings. Panels currently declare data via a selector DSL
(`@bind(nav="nav{portfolio=\"$portfolio_id\"} @asof")`) that the deleted
read-gateway compiled to SQL. Post serviceless-migration there is no DSL
resolver, so every panel is broken. Panels must instead bind the results of
**direct SQL queries**, run against **both Postgres (OLTP) and RisingWave
(streaming)**, routed automatically.

## Scope

- **64 panels** carry the DSL: `oc-plugin-core-app/library-panels/` (63) +
  `oc-plugin-yfinance-app/library-panels/` (1). `core-datasource` has none.
- **8 selector types** in use: `nav @asof`, `flows @window`, `closures @window`,
  `cycles @window`, `instrument @asof`, `instrument @latest`, `events @window`,
  `classification @latest`.
- **Panel inputs** beyond `portfolio_id`: `instrument_id`, `benchmark_ids`,
  `lookback_periods`, `risk_free`, `sample_interval`.

Out of scope: changing metric math (panel bodies are unchanged); RW schema
changes; the `@metric`/output contract.

## Architecture

The compute sidecar (Python, PyInstaller-frozen, pure-Python deps) already
exec's a panel source and calls the `@metric` entrypoint. This design:

1. Restores `@bind` as **registration-only**: each binding value is direct SQL
   (+ params), recorded on the per-exec registry. After `exec`, the endpoint
   runs each binding through a **dual-store router** and calls the entrypoint
   with the resulting Polars frames as kwargs. SQL never executes at decoration
   time (keeps the contract testable + isolated).
2. Adds a **dual-store query path**: one pg8000 connection to RisingWave
   (`:4566`) and one to Postgres `control_db` (`:5432`) — both speak pgwire, so
   the same driver serves both.
3. **Auto-routes** each query to the right store by a discovered
   `table → store` catalog, with an explicit `rw()/pg()` override escape hatch.
4. **Injects panel variables** (`portfolio_id`, …) into the exec namespace so
   binding params can reference them; `core-datasource` delivers them in the
   `/compute` request.

### Component 1 — `@bind` contract (`services/compute/compute/contract.py`)

`Registry` gains `bindings: dict[str, QuerySpec]`. `Contract.bind(**specs)`
records them; `require_complete()` still asserts exactly one `@metric`
entrypoint. A `QuerySpec` is a small NamedTuple:

```python
class QuerySpec(NamedTuple):
    store: str      # "auto" | "rw" | "pg"
    sql: str
    params: tuple
```

Binding value forms (all "direct SQL"):
- **plain string** → `QuerySpec("auto", sql, ())` — no params.
- **tuple** `(sql, *params)` → `QuerySpec("auto", sql, params)`.
- **`rw(sql, *params)` / `pg(sql, *params)`** → explicit store override.
  `rw`/`pg` are 2-line constructors injected into the namespace; they only tag
  the store and capture params — no SQL is hidden, no builder.

Example panel after migration:
```python
@bind(
    nav="SELECT ts, nav FROM portfolio_per_tick WHERE portfolio_id=$1 ORDER BY ts",
    flows=("SELECT business_ts AS ts, amount FROM portfolio_events_log "
           "WHERE portfolio_id=$1 AND business_ts BETWEEN $2 AND $3",
           portfolio_id, window.t0, window.t1),
    pf=pg("SELECT base_currency FROM portfolios WHERE portfolio_id=$1", portfolio_id),
)
@metric(output="scalar")
def total_return(nav, flows, pf):
    ...
```
The first binding has no params; the second uses `portfolio_id`/`window` from
the namespace; the third forces Postgres.

### Component 2 — dual-store router (`services/compute/compute/rwclient.py` → store layer)

A `Store` holds both connections and a discovered catalog:

- **Discovery** (lazy, once per process, refresh-on-miss):
  - RW: `rw_catalog.rw_tables` ∪ `rw_views` ∪ `rw_materialized_views`.
  - PG: `information_schema.tables` (+ `views`) across non-system schemas.
  - Build `table_name → {rw|pg|both}`.
- **Routing** a `QuerySpec`:
  - `store="rw"|"pg"` → that connection, no parse.
  - `store="auto"` → extract FROM/JOIN tables with **sqlglot** (pure-Python),
    look each up. All in one store → route there. Mixed → error (cross-store
    joins are impossible). Unknown table → error with the table name. The one
    overlap, `portfolios` (PG `control_db` → RW via `rw_v6_pub` CDC), defaults
    to **RW** (panels want the analytics mirror; PG `portfolios` is core-app
    CRUD). The overlap default is a single, documented rule.
- Returns a Polars frame (existing `_frame_from`).

`sql(query, *params)` stays in the namespace as a convenience (auto-routed,
body-callable); `rw`/`pg` likewise. Bindings are the primary path.

### Component 3 — variable injection (`services/compute/compute/endpoint.py`)

`build_namespace` adds the panel variables from the request body's `vars` map
as individual names: `portfolio_id`, `instrument_id`, `benchmark_ids`,
`lookback_periods`, `risk_free`, `sample_interval`. Missing vars are injected as
`None` so a panel referencing one it didn't receive fails loudly, not silently.

Request body becomes:
```json
{ "source": "...", "window": {"from": <us>, "to": <us>}, "vars": { "portfolio_id": "...", ... } }
```

Run order: `exec(source)` → `require_complete()` → run each binding via the
router → `entrypoint(**frames)` → shape by `output`.

### Component 4 — `core-datasource`

The datasource backend forwards the Grafana panel's dashboard variables
(`$portfolio_id`, `$instrument_id`, `$benchmark_ids`, …) into the `/compute`
request `vars` map. It already forwards `source` + time range; this adds the
`vars` passthrough sourced from the query/scoped vars.

### Component 5 — selector → SQL catalog (recovery)

The canonical SQL per selector is recovered from the deleted read-gateway
compiler (`services/read-gateway/internal/compile/compiler.go` @ commit
`f6537863`) + `surface.Resolve` (entity→view map) + the current RW/PG schema.
Compiler semantics (org_id scoping dropped; portfolio scope kept):
- `@asof`  → `SELECT <cols> FROM <view> WHERE portfolio_id=$1 [AND ts<=$N] ORDER BY ts ASC`
- `@window`→ `SELECT <cols> FROM <view> WHERE portfolio_id=$1 AND ts BETWEEN $2 AND $3 ORDER BY ts ASC`
- `@latest`→ `SELECT DISTINCT ON (<grain>) <cols> FROM <view> WHERE portfolio_id=$1 ORDER BY <grain>, ts DESC`

The implementation plan produces a table: each of the 8 selectors → its view,
projected columns, store, and predicate template. This table drives the
mechanical panel rewrites.

### Component 6 — panel rewrite

64 files, grouped by selector type for consistency. Mechanical: replace each
DSL selector string with its SQL binding (from the catalog), wire params from
the injected vars/window. Panel bodies (the metric math) are untouched.

## Data flow

```
Grafana panel (vars $portfolio_id, time range)
  → core-datasource  (POST /compute {source, window, vars})
    → compute endpoint
        exec(source)                       # @bind registers specs, @metric registers entrypoint
        for name, spec in bindings:        # router: auto|rw|pg → frame
            frames[name] = store.run(spec)
        result = entrypoint(**frames)      # panel math on Polars frames
        shape(result, output)              # scalar|series|table
      ← frame JSON
```

## Error handling

- Unknown table (auto-route): `400` with the offending table name + "not found
  in rw or pg".
- Cross-store tables in one query: `400` "cannot join across rw and pg".
- Binding SQL error: `400` "binding <name>: <db error>".
- Missing referenced var: surfaces as a Python `NameError`/`None` use → `400`
  source error (loud, not silent wrong data).
- Discovery failure for one store: that store routes nothing; queries needing it
  error clearly; the other store still works.

## Testing

- **contract**: `@bind` registers specs without executing; `require_complete`
  unchanged; multiple `@bind` merge / re-bind rules.
- **router**: table extraction (sqlglot) over the real panel queries; routing
  decisions incl. `portfolios` overlap default + explicit `rw()/pg()` override;
  unknown/cross-store errors.
- **endpoint**: var injection (present/missing); end-to-end register → bind →
  call → frame for one panel of each output mode.
- **catalog parity**: a handful of representative panels produce the same frame
  as the old DSL would have (golden), against a seeded RW+PG.
- **freeze smoke**: sqlglot imports + runs in the PyInstaller one-file binary.
- **panels**: per-group, at least one panel per selector type asserted against
  seeded data.

## Phasing (single plan, ordered tasks)

1. **Compute framework** — `QuerySpec` + `@bind` registration + dual-store
   `Store` (two pg8000 conns) + auto-router (sqlglot, catalog discovery,
   `portfolios`→RW) + `rw()/pg()` + endpoint var injection + request `vars`.
   Tests green.
2. **core-datasource** — forward dashboard vars into `/compute`.
3. **Selector→SQL catalog** — recover from `compiler.go`/`surface` + schema;
   produce the 8-selector mapping table.
4. **Panel rewrites** — 64 files by selector group (RW MV reads, `portfolios`,
   any PG plugin-schema reads).
5. **Verify** — golden parity on representative panels; live render in the app
   for one panel per output mode.

## Open items (resolved in planning)

- Exact `surface` entity→view map + per-view projected columns (recover in
  Phase 3).
- Whether any panel legitimately needs PG `portfolios` (authoritative) vs the RW
  mirror — decide per panel during rewrite; default RW.
- How `core-datasource` enumerates which vars to forward (fixed allowlist vs all
  scoped vars) — Phase 2 detail.
