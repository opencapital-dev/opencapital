# Panel Direct-SQL Bindings — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the dead read-gateway DSL in the 64 library panels with direct-SQL `@bind` bindings that run against both RisingWave and Postgres, auto-routed by a discovered table→store catalog.

**Architecture:** The compute sidecar restores `@bind` as registration-only (records `name → QuerySpec`). After `exec`, the endpoint runs each binding through a dual-store router (one pg8000 conn to RW `:4566`, one to PG `control_db` `:5432`), binds the resulting Polars frames to the entrypoint's args, then calls it. Auto-routing parses the query's tables (sqlglot) and looks them up in a catalog discovered from both stores; `rw()/pg()` override when needed. Panel input variables (`$portfolio_id`, …) are already text-substituted into the source by `core-datasource` before forwarding, so no namespace injection is added.

**Tech Stack:** Python 3 (PyInstaller-frozen sidecar), pg8000 (pure-Python pgwire), polars, sqlglot (pure-Python SQL parser), Go (core-datasource plugin), Rust (Tauri host).

## Global Constraints

- Pure-Python deps only in the compute sidecar (PyInstaller one-file freeze): pg8000, polars, sqlglot. No SQLAlchemy/ORM/C-ext query libs.
- Both stores reached with the **same** pg8000 driver (PG + RW both speak pgwire); only the DSN differs.
- The single cross-store table is `portfolios` (PG `control_db` → RW via `rw_v6_pub` CDC); on an `auto` route it resolves to **RW**.
- Panel variables arrive via `core-datasource`'s existing `substituteVars(code, vars)` text substitution (`$portfolio_id`, `${instrument_id}`, `$benchmark_ids`, `${lookback_periods}`, `${risk_free}`, `${sample_interval}`). `$var` MUST land inside a Python string/number literal so it becomes a bound SQL param — never spliced raw into SQL text.
- Time range arrives as the injected `window` (`Window(t0, t1)`, int microseconds); `@window` bindings use `window.t0`/`window.t1` as params.
- `@metric(output=...)` contract and panel math bodies are unchanged.
- **Deviation from the spec (2026-06-21 design):** the spec's "inject vars into the exec namespace" + "core-datasource sends vars" components are dropped — `core-datasource` already substitutes vars into the source text, so the simpler existing path is reused.

---

## File Structure

- `services/compute/compute/contract.py` — MODIFY: add `QuerySpec`, `bind` decorator + registry bindings.
- `services/compute/compute/store.py` — CREATE: dual-store `Store` (two pg8000 conns), catalog discovery, `route()`. (Generalizes today's `rwclient.py`; keep `rwclient.query/_frame_from` as helpers it calls.)
- `services/compute/compute/router.py` — CREATE: `tables_in(sql)` (sqlglot) + `decide_store(tables, catalog, declared)`.
- `services/compute/compute/endpoint.py` — MODIFY: run bindings, inject `bind`/`rw`/`pg`, dual DSN.
- `services/compute/compute/server.py` — MODIFY: read `POSTGRES_DSN`; pass both DSNs.
- `services/compute/pyproject.toml` + `services/compute/compute.spec` (or Makefile freeze hiddenimports) — MODIFY: add `sqlglot`.
- `opencapital/opencapital-app/src-tauri/src/dataplane.rs` (compute spawn) — MODIFY: set `POSTGRES_DSN` env on the sidecar.
- `docs/superpowers/reference/selector-sql-catalog.md` — CREATE: the 8-selector → view/columns/store/predicate table.
- `oc-plugin-core-app/library-panels/*.py` (63) + `oc-plugin-yfinance-app/library-panels/*.py` (1) — MODIFY: DSL → direct-SQL `@bind`.

---

## Task 1: `QuerySpec` + `@bind` registration (contract)

**Files:**
- Modify: `services/compute/compute/contract.py`
- Test: `services/compute/tests/test_contract.py`

**Interfaces:**
- Produces: `class QuerySpec(NamedTuple): store: str; sql: str; params: tuple`; `Registry.bindings: dict[str, QuerySpec]`; `Contract.bind(**specs) -> Callable` (decorator returning the fn unchanged); helpers `to_spec(value) -> QuerySpec` where a `str` → `QuerySpec("auto", str, ())`, a `tuple` → `QuerySpec("auto", t[0], tuple(t[1:]))`, a `QuerySpec` → itself.
- Consumes: existing `Registry`, `Contract`, `make_contract`, `ContractError`.

- [ ] **Step 1: Write failing tests**

```python
# services/compute/tests/test_contract.py  (add)
from compute.contract import make_contract, QuerySpec, to_spec, ContractError

def test_bind_records_specs_normalizing_value_forms():
    c = make_contract()
    @c.bind(a="SELECT 1", b=("SELECT $1", 7), c=QuerySpec("pg", "SELECT 2", ()))
    @c.metric(output="table")
    def f(a, b, c):
        return a
    binds = c.registry.bindings
    assert binds["a"] == QuerySpec("auto", "SELECT 1", ())
    assert binds["b"] == QuerySpec("auto", "SELECT $1", (7,))
    assert binds["c"] == QuerySpec("pg", "SELECT 2", ())

def test_bind_alone_without_metric_is_incomplete():
    c = make_contract()
    @c.bind(a="SELECT 1")
    def f(a):
        return a
    try:
        c.registry.require_complete()
        assert False, "expected ContractError"
    except ContractError:
        pass

def test_to_spec_rejects_unknown_store():
    try:
        to_spec(QuerySpec("mysql", "SELECT 1", ()))
        assert False
    except ContractError:
        pass
```

- [ ] **Step 2: Run, verify fail**

Run: `cd services/compute && python -m pytest tests/test_contract.py -q`
Expected: FAIL (`ImportError: QuerySpec` / `to_spec`).

- [ ] **Step 3: Implement**

In `contract.py` add (near the top, after imports):

```python
from typing import Iterable

_STORES: frozenset[str] = frozenset({"auto", "rw", "pg"})

class QuerySpec(NamedTuple):
    store: str       # "auto" | "rw" | "pg"
    sql: str
    params: tuple

def to_spec(value) -> "QuerySpec":
    """Normalize a binding value into a QuerySpec.

    str         -> auto-routed, no params
    (sql, *p)   -> auto-routed, positional params
    QuerySpec   -> validated as-is (from rw()/pg())
    """
    if isinstance(value, QuerySpec):
        spec = value
    elif isinstance(value, str):
        spec = QuerySpec("auto", value, ())
    elif isinstance(value, tuple) and value and isinstance(value[0], str):
        spec = QuerySpec("auto", value[0], tuple(value[1:]))
    else:
        raise ContractError(f"invalid @bind value {value!r}; want str, (sql, *params), or rw()/pg()")
    if spec.store not in _STORES:
        raise ContractError(f"invalid store {spec.store!r}; expected one of {sorted(_STORES)}")
    if not isinstance(spec.sql, str) or not spec.sql.strip():
        raise ContractError("@bind sql must be a non-empty string")
    return spec
```

Add `bindings` to `Registry`:

```python
@dataclass(slots=True)
class Registry:
    entrypoint: Callable | None = None
    output: OutputMode | None = None
    bindings: dict = field(default_factory=dict)   # name -> QuerySpec

    def require_complete(self) -> None:
        if self.entrypoint is None:
            raise ContractError("no @metric entrypoint declared in source")
```

Add `bind` to `Contract`:

```python
    def bind(self, **specs) -> Callable[[Callable], Callable]:
        """Record one QuerySpec per binding name; returns the fn unchanged.

        Pure registration — no SQL runs here. The endpoint executes the specs
        after exec and passes the resulting frames to the entrypoint as kwargs.
        """
        reg = self.registry
        normalized = {name: to_spec(val) for name, val in specs.items()}
        reg.bindings.update(normalized)
        def decorate(fn: Callable) -> Callable:
            return fn
        return decorate
```

- [ ] **Step 4: Run, verify pass**

Run: `cd services/compute && python -m pytest tests/test_contract.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/compute/compute/contract.py services/compute/tests/test_contract.py
git commit -m "feat(compute): QuerySpec + @bind registration (direct-SQL bindings)"
```

---

## Task 2: dual-store `Store` + `rw()`/`pg()` constructors

**Files:**
- Create: `services/compute/compute/store.py`
- Modify: `services/compute/compute/rwclient.py` (reuse `connect`, `query`, `_frame_from`)
- Test: `services/compute/tests/test_store.py`

**Interfaces:**
- Produces: `rw(sql, *params) -> QuerySpec("rw", ...)`, `pg(sql, *params) -> QuerySpec("pg", ...)`; `class Store` with `__init__(rw_dsn, pg_dsn)`, `catalog() -> dict[str,str]` (table → "rw"|"pg"|"both", lazy+cached), `run(spec: QuerySpec) -> pl.DataFrame`, `close()`.
- Consumes: `rwclient.connect/query`, `router.decide_store` (Task 3), `contract.QuerySpec`.

- [ ] **Step 1: Write failing tests** (catalog + routing use a fake conn; no live DB)

```python
# services/compute/tests/test_store.py
import polars as pl
from compute.store import rw, pg, Store
from compute.contract import QuerySpec

def test_rw_pg_constructors():
    assert rw("SELECT 1", 5) == QuerySpec("rw", "SELECT 1", (5,))
    assert pg("SELECT 2") == QuerySpec("pg", "SELECT 2", ())

def test_run_routes_explicit_store(monkeypatch):
    calls = []
    s = Store.__new__(Store)          # bypass real connections
    s._rw = object(); s._pg = object()
    s._catalog = {"portfolio_per_tick": "rw"}
    def fake_query(conn, sql, params):
        calls.append((conn, sql, params)); return pl.DataFrame({"x": [1]})
    monkeypatch.setattr("compute.store.rwclient.query", fake_query)
    s.run(QuerySpec("rw", "SELECT 1", ()))
    assert calls[0][0] is s._rw
    s.run(QuerySpec("pg", "SELECT 1", ()))
    assert calls[1][0] is s._pg
```

- [ ] **Step 2: Run, verify fail**

Run: `cd services/compute && python -m pytest tests/test_store.py -q`
Expected: FAIL (`ModuleNotFoundError: compute.store`).

- [ ] **Step 3: Implement** `services/compute/compute/store.py`

```python
"""Dual-store query layer: one pg8000 connection to RisingWave, one to
Postgres control_db. Both speak pgwire, so rwclient's query/_frame_from serve
both. A QuerySpec carries store="auto"|"rw"|"pg"; auto is resolved by router.
"""
from __future__ import annotations
import polars as pl
from compute import rwclient
from compute.contract import QuerySpec
from compute.router import tables_in, decide_store

def rw(sql: str, *params) -> QuerySpec:
    return QuerySpec("rw", sql, tuple(params))

def pg(sql: str, *params) -> QuerySpec:
    return QuerySpec("pg", sql, tuple(params))

_RW_CATALOG_SQL = (
    "SELECT name FROM rw_catalog.rw_tables "
    "UNION ALL SELECT name FROM rw_catalog.rw_materialized_views "
    "UNION ALL SELECT name FROM rw_catalog.rw_views"
)
_PG_CATALOG_SQL = (
    "SELECT table_name AS name FROM information_schema.tables "
    "WHERE table_schema NOT IN ('pg_catalog','information_schema')"
)

class Store:
    def __init__(self, rw_dsn: str, pg_dsn: str | None):
        self._rw = rwclient.connect(rw_dsn)
        self._pg = rwclient.connect(pg_dsn) if pg_dsn else None
        self._catalog: dict[str, str] | None = None

    def catalog(self) -> dict[str, str]:
        if self._catalog is None:
            cat: dict[str, str] = {}
            for name in rwclient.query(self._rw, _RW_CATALOG_SQL)["name"].to_list():
                cat[name] = "rw"
            if self._pg is not None:
                for name in rwclient.query(self._pg, _PG_CATALOG_SQL)["name"].to_list():
                    cat[name] = "both" if cat.get(name) == "rw" else "pg"
            self._catalog = cat
        return self._catalog

    def run(self, spec: QuerySpec) -> pl.DataFrame:
        store = spec.store
        if store == "auto":
            store = decide_store(tables_in(spec.sql), self.catalog())
        conn = self._rw if store == "rw" else self._pg
        if conn is None:
            raise RuntimeError(f"no connection for store {store!r} (postgres DSN unset?)")
        return rwclient.query(conn, spec.sql, spec.params)

    def close(self) -> None:
        for c in (self._rw, self._pg):
            try:
                if c is not None:
                    c.close()
            except Exception:
                pass
```

- [ ] **Step 4: Run, verify pass**

Run: `cd services/compute && python -m pytest tests/test_store.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/compute/compute/store.py services/compute/tests/test_store.py
git commit -m "feat(compute): dual-store Store + rw()/pg() constructors"
```

---

## Task 3: auto-router (sqlglot table extraction + store decision)

**Files:**
- Create: `services/compute/compute/router.py`
- Modify: `services/compute/pyproject.toml` (add `sqlglot`)
- Test: `services/compute/tests/test_router.py`

**Interfaces:**
- Produces: `tables_in(sql: str) -> set[str]` (bare table names, lowercased, schema stripped); `decide_store(tables: set[str], catalog: dict[str,str]) -> str` returning "rw"|"pg".
- Consumes: nothing from sibling tasks.

- [ ] **Step 1: Write failing tests**

```python
# services/compute/tests/test_router.py
import pytest
from compute.router import tables_in, decide_store

def test_tables_in_simple_and_join():
    assert tables_in("SELECT ts, nav FROM portfolio_per_tick WHERE portfolio_id=$1") == {"portfolio_per_tick"}
    assert tables_in("SELECT * FROM a JOIN b ON a.id=b.id") == {"a", "b"}
    assert tables_in("SELECT * FROM yfinance.gw_classification") == {"gw_classification"}

def test_decide_store_unambiguous():
    cat = {"portfolio_per_tick": "rw", "gw_classification": "pg"}
    assert decide_store({"portfolio_per_tick"}, cat) == "rw"
    assert decide_store({"gw_classification"}, cat) == "pg"

def test_decide_store_portfolios_overlap_defaults_rw():
    assert decide_store({"portfolios"}, {"portfolios": "both"}) == "rw"

def test_decide_store_unknown_table_errors():
    with pytest.raises(ValueError, match="not found"):
        decide_store({"nope"}, {"portfolio_per_tick": "rw"})

def test_decide_store_cross_store_errors():
    cat = {"a": "rw", "b": "pg"}
    with pytest.raises(ValueError, match="across"):
        decide_store({"a", "b"}, cat)
```

- [ ] **Step 2: Run, verify fail**

Run: `cd services/compute && python -m pytest tests/test_router.py -q`
Expected: FAIL (`ModuleNotFoundError: compute.router` / `sqlglot`).

- [ ] **Step 3: Add dep + implement**

Add to `services/compute/pyproject.toml` `[project] dependencies`: `"sqlglot>=25,<27"`. Then `cd services/compute && pip install -e .` (or `uv pip install sqlglot`).

`services/compute/compute/router.py`:

```python
"""Route an auto-store query to rw or pg by the tables it reads."""
from __future__ import annotations
import sqlglot
from sqlglot import exp

def tables_in(sql: str) -> set[str]:
    """Bare table names referenced by *sql* (schema/db qualifiers stripped, lowercased)."""
    try:
        tree = sqlglot.parse_one(sql, read="postgres")
    except Exception as exc:
        raise ValueError(f"unparseable SQL: {exc}") from exc
    return {t.name.lower() for t in tree.find_all(exp.Table) if t.name}

# The lone CDC-mirrored table; on auto it resolves to the RW analytics mirror.
_OVERLAP_DEFAULT = {"portfolios": "rw"}

def decide_store(tables: set[str], catalog: dict[str, str]) -> str:
    """Pick "rw" or "pg" for a query reading *tables*; raise on unknown/cross-store."""
    stores: set[str] = set()
    for t in tables:
        loc = catalog.get(t)
        if loc is None:
            raise ValueError(f"table {t!r} not found in rw or pg catalog")
        if loc == "both":
            stores.add(_OVERLAP_DEFAULT.get(t, "rw"))
        else:
            stores.add(loc)
    if len(stores) > 1:
        raise ValueError(f"query reads across stores {sorted(stores)}; cross-store joins unsupported")
    return stores.pop() if stores else "rw"
```

- [ ] **Step 4: Run, verify pass**

Run: `cd services/compute && python -m pytest tests/test_router.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/compute/compute/router.py services/compute/tests/test_router.py services/compute/pyproject.toml
git commit -m "feat(compute): auto-router (sqlglot table extraction + store decision)"
```

---

## Task 4: endpoint runs bindings + dual DSN wiring

**Files:**
- Modify: `services/compute/compute/endpoint.py`
- Modify: `services/compute/compute/server.py`
- Test: `services/compute/tests/test_endpoint.py`

**Interfaces:**
- Consumes: `Store` (Task 2), `to_spec`/`bind` (Task 1), `rw`/`pg` (Task 2).
- Produces: `build_namespace(contract, window, store)` injects `bind`, `rw`, `pg`, plus a `sql`/`pgsql` convenience; `run_compute(body, rw_dsn, pg_dsn)`.

- [ ] **Step 1: Write failing test** (end-to-end with a fake Store)

```python
# services/compute/tests/test_endpoint.py  (add)
import polars as pl
from compute.endpoint import run_compute
import compute.endpoint as ep

class _FakeStore:
    def __init__(self, *a, **k): pass
    def run(self, spec):
        return pl.DataFrame({"ts": [1, 2], "nav": [100.0, 110.0]})
    def close(self): pass

def test_run_compute_binds_frames_to_entrypoint(monkeypatch):
    monkeypatch.setattr(ep, "Store", _FakeStore)
    src = (
        "@bind(nav=rw('SELECT ts, nav FROM portfolio_per_tick WHERE portfolio_id=$1', 'p1'))\n"
        "@metric(output='scalar')\n"
        "def m(nav):\n"
        "    return float(nav['nav'][-1])\n"
    )
    out = run_compute({"source": src, "window": {"from": 0, "to": 100}}, "rwdsn", "pgdsn")
    assert out["output"] == "scalar"
    assert out["rows"] == [[110.0]]
```

- [ ] **Step 2: Run, verify fail**

Run: `cd services/compute && python -m pytest tests/test_endpoint.py::test_run_compute_binds_frames_to_entrypoint -q`
Expected: FAIL (`run_compute()` takes 2 args / `Store` not imported / `bind` not in ns).

- [ ] **Step 3: Implement**

In `endpoint.py`: import `from compute.store import Store, rw, pg`. Update `build_namespace`:

```python
def build_namespace(contract, window: Window, store) -> dict:
    ns: dict = {name: getattr(metrics, name) for name in metrics.__all__}
    ns.update(_CURATED_STDLIB)
    ns["metric"] = contract.metric
    ns["bind"] = contract.bind
    ns["window"] = window
    ns["pl"] = pl
    ns["rw"] = rw
    ns["pg"] = pg
    ns["sql"] = lambda q, *p: store.run(_auto_spec(q, p))      # auto-routed convenience
    return ns
```

Add a tiny helper `_auto_spec(q, p)` returning `QuerySpec("auto", q, tuple(p))` (import `QuerySpec` from contract). Rewrite `run_compute`:

```python
def run_compute(body: dict, rw_dsn: str, pg_dsn: str | None) -> dict:
    source, window = _parse_body(body)
    store = Store(rw_dsn, pg_dsn)
    try:
        contract = make_contract()
        ns = build_namespace(contract, window, store)
        try:
            exec(source, ns)  # noqa: S102
        except ContractError as exc:
            raise ComputeError(400, str(exc)) from exc
        except Exception as exc:
            raise ComputeError(400, f"source error: {exc}") from exc

        reg = contract.registry
        try:
            reg.require_complete()
        except ContractError as exc:
            raise ComputeError(400, str(exc)) from exc

        frames = {}
        for name, spec in reg.bindings.items():
            try:
                frames[name] = store.run(spec)
            except Exception as exc:
                raise ComputeError(400, f"binding {name!r}: {exc}") from exc
        try:
            result = reg.entrypoint(**frames)
        except Exception as exc:
            raise ComputeError(400, f"entrypoint error: {exc}") from exc

        return _to_frame(reg.output, result)
    finally:
        store.close()
```

(Keep `run_plan` working: it builds a namespace with a no-op store — pass a stub whose `run` returns `pl.DataFrame()`, or guard `build_namespace` to tolerate `store=None` for /plan by injecting no-op `sql`/`rw`/`pg`. Simplest: a `_NoopStore` with `run` returning `pl.DataFrame()`.)

In `server.py`: add `_DEFAULT_PG_DSN = "postgres://postgres@127.0.0.1:5432/control_db?sslmode=disable"`; read `POSTGRES_DSN` env into `self.server.pg_dsn`; call `run_compute(body, self.server.dsn, self.server.pg_dsn)`.

- [ ] **Step 4: Run, verify pass + full suite**

Run: `cd services/compute && python -m pytest tests/test_endpoint.py tests/test_server.py -q`
Expected: PASS. Fix `run_plan`/`test_server` fallout if any.

- [ ] **Step 5: Commit**

```bash
git add services/compute/compute/endpoint.py services/compute/compute/server.py services/compute/tests/test_endpoint.py
git commit -m "feat(compute): run @bind specs via dual-store; POSTGRES_DSN wiring"
```

---

## Task 5: freeze sqlglot into the sidecar

**Files:**
- Modify: `services/compute/compute.spec` or the freeze hiddenimports (whichever the Makefile `compute-freeze` uses)
- Test: `services/compute/tests/test_freeze_smoke.py`

- [ ] **Step 1: Extend the freeze smoke test** to import + use sqlglot inside the frozen binary:

```python
# add to test_freeze_smoke.py
def test_frozen_binary_parses_sql(compute_binary):
    # the frozen binary must be able to route an auto query (sqlglot present)
    out = _post(compute_binary, "/plan", {"source": "@metric(output='table')\ndef f():\n    return pl.DataFrame()\n"})
    assert out.status == 200
```

(If the existing smoke harness differs, mirror its pattern; the assertion is that a build including sqlglot freezes + runs.)

- [ ] **Step 2: Run freeze, verify fail/pass**

Run: `cd /Users/ignacioballester/trading-code/opencapital && make compute-freeze && cd services/compute && python -m pytest tests/test_freeze_smoke.py -q`
Expected: initially may FAIL with `ModuleNotFoundError: sqlglot` in the frozen binary.

- [ ] **Step 3: Add sqlglot to the freeze**

In the PyInstaller spec/hiddenimports add `sqlglot` (and `--collect-submodules sqlglot` if needed — sqlglot lazy-imports dialects). Confirm with `make compute-freeze`.

- [ ] **Step 4: Re-run, verify pass**

Run: `make compute-freeze && cd services/compute && python -m pytest tests/test_freeze_smoke.py -q`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add services/compute/compute.spec services/compute/tests/test_freeze_smoke.py
git commit -m "build(compute): bundle sqlglot in the PyInstaller freeze"
```

---

## Task 6: pass POSTGRES_DSN to the sidecar (Tauri)

**Files:**
- Modify: `opencapital/opencapital-app/src-tauri/src/dataplane.rs` (compute sidecar spawn — where `RISINGWAVE_DSN` is set)
- Test: `opencapital/opencapital-app/src-tauri` cargo test (assert the env builder includes both DSNs)

- [ ] **Step 1: Find the spawn.** Locate where the compute sidecar is launched and `RISINGWAVE_DSN` is set (grep `RISINGWAVE_DSN`). Note the PG coords already used by `bootstrap_control_db` (host/port/`control_db`).

- [ ] **Step 2: Write/extend a test** asserting the sidecar env map contains `POSTGRES_DSN=postgres://postgres@127.0.0.1:<pg_port>/control_db?sslmode=disable` alongside `RISINGWAVE_DSN`. (If env assembly isn't unit-testable, add a small `fn compute_env(cfg) -> Vec<(String,String)>` seam and test it.)

- [ ] **Step 3: Set the env** on the compute `Command` next to `RISINGWAVE_DSN`.

- [ ] **Step 4: Run** `cargo test` for the seam; `cargo build`. Expected: PASS/clean.

- [ ] **Step 5: Commit**

```bash
git add opencapital-app/src-tauri/src/dataplane.rs
git commit -m "feat(desktop): pass POSTGRES_DSN to the compute sidecar"
```

---

## Task 7: recover the selector → SQL catalog

**Files:**
- Create: `docs/superpowers/reference/selector-sql-catalog.md`

This task produces no code — it produces the authoritative mapping the panel rewrites consume. It is a gate: the rewrites cannot start until each selector's view, columns, store, and predicate are pinned against the **current** schema.

- [ ] **Step 1: Extract the old mapping.** From `git show f6537863:services/read-gateway/internal/surface/surface.go`:
  - `nav→e_nav`, `flows→e_flows`, `cash→e_cash`, `closures→e_closures`, `cycles→e_cycles`, `events→e_events`, `instrument→e_instrument`, `portfolio→e_portfolio`, `price→e_price`, `instruments_used→instruments_catalog`, `ohlcv→prices_ohlcv`.
  - Grains (for `@latest` DISTINCT ON): `closures/cycles/instrument→[portfolio,instrument]`, `nav/flows/portfolio→[portfolio]`, `cash→[portfolio,currency]`.
  - From `git show f6537863:services/read-gateway/internal/compile/compiler.go`: `@asof`/`@window` → `SELECT <cols> FROM <view> WHERE portfolio_id=$1 [AND ts BETWEEN $2 AND $3] ORDER BY ts ASC`; `@latest` → `SELECT DISTINCT ON (<grain>) <cols> FROM <view> WHERE portfolio_id=$1 ORDER BY <grain>, ts DESC`.

- [ ] **Step 2: Verify each view exists in the CURRENT schema** (the migration may have renamed `e_*`). For each selector run, against a live app's RW (`psql -h localhost -p 4566 -d dev`) and PG (`psql -h localhost -p 5432 -d control_db`):
  - `SELECT * FROM <candidate_view> LIMIT 0;` to confirm the view + its columns. If `e_flows`/`e_closures`/etc. don't exist, map to the actual current source (e.g. `portfolio_per_tick`, `closures_per_event`, `portfolio_events_log`) using `dataplane/risingwave/schemas/**`. `classification` resolves to PG `yfinance.gw_classification` (a PG read — exercises the dual-store path).

- [ ] **Step 3: Write the catalog table** — for each of the 8 selectors actually used (`nav@asof`, `flows@window`, `closures@window`, `cycles@window`, `instrument@asof`, `instrument@latest`, `events@window`, `classification@latest`): selector → store → view → projected columns → SQL template (with `$1=portfolio_id`, `$2/$3=window`, and `instrument_id` where the grain needs it). One worked example each.

- [ ] **Step 4: Sanity-run each template** against seeded data in the live app; confirm a non-empty frame for a portfolio with data.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/reference/selector-sql-catalog.md
git commit -m "docs(reference): selector->SQL catalog for panel rewrites"
```

---

## Task 8: rewrite core-app panels — `@asof`/`@window` group (most panels)

**Files:**
- Modify: `oc-plugin-core-app/library-panels/*.py` that use only `nav@asof`, `flows@window`, `closures@window`, `cycles@window`, `events@window`, `instrument@asof`.
- Test: `oc-plugin-core-app` — add `library-panels/tests/test_panels_compile.py` (or the repo's panel-test location) that execs each rewritten panel against a stub `bind/metric/rw/pg/window/pl` namespace and asserts the contract registers + bindings parse.

**The transformation rule (apply per panel, using the Task 7 catalog):**
1. Replace the `@bind(name="<selector>{...} <modifier>")` strings with `@bind(name=<sql-binding>)` from the catalog. The binding value is a tuple `("<SQL with $1[/$2/$3]>", "$portfolio_id"[, window.t0, window.t1])`. Use `"${instrument_id}"` as the param where the selector carries an `instrument` matcher.
2. Keep the function signature (`def total_return(nav, flows): ...`) and body unchanged.
3. Window selectors pass `window.t0, window.t1`; asof selectors pass only `"$portfolio_id"`.

**Worked example** — `total_return.py`:

```python
@bind(
    nav=("SELECT ts, nav FROM portfolio_per_tick WHERE portfolio_id=$1 ORDER BY ts", "$portfolio_id"),
    flows=("SELECT business_ts AS ts, amount FROM portfolio_events_log "
           "WHERE portfolio_id=$1 AND business_ts BETWEEN $2 AND $3 ORDER BY business_ts",
           "$portfolio_id", window.t0, window.t1),
)
@metric(output="scalar")
def total_return(nav, flows):
    t0, t1 = window
    effective_start = t0 if nav.is_empty() else max(t0, nav["ts"][0])
    flow_ts = [r[0] for r in flows.select("ts").rows() if r[0] > effective_start]
    grid = build_grid(effective_start, t1, "1d")
    cum = cumulative_twr(nav, flow_ts, effective_start, grid)
    return cum[-1][1] if cum else None
```

(Exact view/column names come from Task 7 — `portfolio_per_tick`/`portfolio_events_log` are illustrative; use the catalog's verified names.)

- [ ] **Step 1:** Write `test_panels_compile.py` — for each panel file in this group, exec the source in a stub namespace (`bind`/`metric` from a fresh `make_contract()`, `rw`/`pg` from `store`, `window=Window(0,1)`, `pl`, plus metric helpers) and assert: exactly one entrypoint, all bindings normalize via `to_spec`, every binding's SQL parses with `tables_in()` and `decide_store()` against a catalog fixture (`{view: store}` from Task 7). Run, verify it fails for the not-yet-rewritten files.
- [ ] **Step 2:** Rewrite each file in the group per the rule. Run the test after each handful; verify it passes.
- [ ] **Step 3:** Straggler grep is empty: `grep -rlE "@asof|@window|@latest|\{portfolio=" oc-plugin-core-app/library-panels` returns nothing for this group's files.
- [ ] **Step 4:** Commit.

```bash
git add oc-plugin-core-app/library-panels/ oc-plugin-core-app/library-panels/tests/test_panels_compile.py
git commit -m "refactor(core-app panels): @asof/@window DSL -> direct-SQL bindings"
```

---

## Task 9: rewrite core-app `@latest`/`classification` + yfinance panel

**Files:**
- Modify: remaining `oc-plugin-core-app/library-panels/*.py` using `instrument@latest` / `classification@latest`.
- Modify: `oc-plugin-yfinance-app/library-panels/*.py` (1 file).

- [ ] **Step 1:** Extend `test_panels_compile.py` to cover these files (same assertions; `classification` must route to `pg` per the catalog).
- [ ] **Step 2:** Rewrite using the `@latest` template:

```python
@bind(
    classification=pg(
        "SELECT DISTINCT ON (portfolio, instrument_id) portfolio, instrument_id, sector, industry "
        "FROM gw_classification WHERE portfolio=$1 ORDER BY portfolio, instrument_id, ts DESC",
        "$portfolio_id"),
)
@metric(output="table")
def passthrough(classification):
    return classification
```

(Use the catalog's verified columns/grain. `classification` is forced to PG via `pg(...)`; the RW `@latest` ones use a plain tuple/`rw(...)`.)

- [ ] **Step 3:** Run `test_panels_compile.py`; the full straggler grep across BOTH plugins' `library-panels/` for `@asof|@window|@latest|{portfolio=` returns nothing.
- [ ] **Step 4:** Commit.

```bash
git add oc-plugin-core-app/library-panels/ oc-plugin-yfinance-app/library-panels/
git commit -m "refactor(panels): @latest/classification DSL -> direct-SQL bindings"
```

---

## Task 10: end-to-end verification (golden + live)

**Files:**
- Create: `services/compute/tests/test_panel_golden.py`

- [ ] **Step 1:** Seed a known portfolio in PG + RW (reuse `scripts/rw_golden_nav.sh` for the RW pipeline). Pick 3 representative panels: one `scalar` (`total_return`), one `series` (`cumulative_return_series`), one `table` (`instrument_quantity_price`).
- [ ] **Step 2:** Write `test_panel_golden.py` that POSTs each rewritten panel source (with vars pre-substituted, as core-datasource would) to a live compute sidecar (or calls `run_compute` directly against the seeded stores) and asserts the frame matches the expected golden values for the seeded data.
- [ ] **Step 3:** Run: `cd services/compute && python -m pytest tests/test_panel_golden.py -q`. Expected: PASS.
- [ ] **Step 4:** Live render: build the app (`make app`), launch, open one panel of each output mode for the seeded portfolio; confirm data renders (no `source error` / `binding` error in `~/.opencapital/instance/logs/grafana.log`).
- [ ] **Step 5:** Commit.

```bash
git add services/compute/tests/test_panel_golden.py
git commit -m "test(compute): golden parity for direct-SQL panels"
```

---

## Self-Review

**Spec coverage:** new `@bind` (T1), dual-store + `rw()/pg()` (T2), auto-route + sqlglot + `portfolios`→RW (T3), endpoint runs bindings + dual DSN (T4), freeze (T5), Tauri PG DSN (T6), selector catalog recovery (T7), 64 panel rewrites (T8–T9), verification (T10). Spec's var-injection/core-datasource components intentionally dropped (Global Constraints note) — reuse existing `substituteVars`. **Covered.**

**Placeholder scan:** view/column names in T8–T9 examples are explicitly marked illustrative and sourced from the T7 catalog (a real gating task), not placeholders. No TBD/"handle errors"/"similar to" left.

**Type consistency:** `QuerySpec(store, sql, params)`, `to_spec`, `Store.run(spec)`, `tables_in`, `decide_store`, `rw`/`pg`, `build_namespace(contract, window, store)`, `run_compute(body, rw_dsn, pg_dsn)` used consistently across T1–T4 and T8–T10.
