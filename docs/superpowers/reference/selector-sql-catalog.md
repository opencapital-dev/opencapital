# Selector → SQL catalog (panel direct-SQL rewrites)

Authoritative mapping for rewriting the library-panel DSL into direct-SQL `@bind`
bindings. Recovered from the deleted read-gateway compiler
(`services/read-gateway/internal/{surface,compile}` @ commit `f6537863`) and
**verified against the live schema** on 2026-06-21 (RW `:4566/dev`, PG
`:5432/control_db`).

## Verified facts (live schema)

- The entity views still exist in RW with these columns (scope column is
  **`portfolio`**, time column is **`ts`** int-µs; instrument-grained views also
  have **`instrument`**):
  - `e_nav(portfolio, ts, value)`
  - `e_flows(portfolio, ts, flow_type, amt)`
  - `e_closures(portfolio, instrument, ts, realized_pnl, holding_seconds)`
  - `e_cycles(portfolio, instrument, ts, pnl_base, duration_sec, was_re_entry)`
  - `e_events(portfolio, instrument, ts, event_type, payload, base_currency)`
  - `e_instrument(portfolio, instrument, ts, direction, quantity, currency, avg_cost_avg_native, avg_cost_avg_base, last_price, realized_equity_avg_native, realized_equity_avg_base, realized_forex_avg_base, unrealized_equity_avg_native, unrealized_equity_avg_base, unrealized_forex_avg_base, lot_count, position_size_base, current_value, unrealized_pnl)`
- `classification` → PG `yfinance.gw_classification(portfolio, instrument_id, ts, sector, industry)` (per the yfinance v0.2.0 migration; the view is **created lazily by the yfinance plugin on startup**, so it may be absent until yfinance has run — the auto-router will route it to PG once present). This is the one PG-routed selector; all others are RW.

## Translation rules

The old DSL projected **all view columns** (no explicit projection appeared in
any panel), so the rewrite uses `SELECT *` to preserve the exact frame shape the
unchanged panel bodies expect.

- **Scope:** always `WHERE portfolio = $1`, param = `"$portfolio_id"` (a quoted
  Python literal so `core-datasource`'s `substituteVars` turns it into a bound
  SQL string param — NEVER splice the value raw into SQL).
- **Matchers:** every label/comparison inside the selector braces (other than
  `portfolio`, which is the scope) becomes an extra `AND` clause, with its value
  as the next positional param ($2, $3, …). Three forms occur in the panels:
  - **string equality** `event_type="TRADE"` → `AND event_type = $N`, param the
    literal `"TRADE"` (a quoted Python str).
  - **variable equality** `instrument="${instrument_id}"` → `AND instrument = $N`,
    param `"${instrument_id}"` (quoted literal → substituted to a bound param).
  - **numeric comparison** `quantity != 0` → `AND quantity != $N`, param `0`
    (a bare Python int, NOT quoted). Operators seen: `=`, `!=`, `<`, `>`, `<=`, `>=`.
  Param ORDER in the binding tuple: `"$portfolio_id"` first, then each matcher's
  value left-to-right, then (for `@window`) `window.t0, window.t1`.
- **`@asof`** → `SELECT * FROM <view> WHERE <scope+matchers> ORDER BY ts ASC`
  (full ordered series; the old compiler did NOT bound `@asof` by the window —
  panels window in-code via the injected `window`).
- **`@window`** → `SELECT * FROM <view> WHERE <scope+matchers> AND ts BETWEEN $N AND $M ORDER BY ts ASC`, params append `window.t0, window.t1` (Python refs, NOT `$`-vars).
- **`@latest`** → `SELECT DISTINCT ON (<grain>) * FROM <view> WHERE <scope+matchers> ORDER BY <grain>, ts DESC`. Grains: `nav/flows/portfolio → (portfolio)`; `closures/cycles/instrument → (portfolio, instrument)`; `classification → (portfolio, instrument_id)`.

## The 8 selectors in use (store, view, template)

Params shown in order; `pid="$portfolio_id"`, `iid="${instrument_id}"`,
`w0=window.t0`, `w1=window.t1`.

| Selector (DSL) | Store | Binding (SQL, params) |
|---|---|---|
| `nav{portfolio} @asof` | rw | `("SELECT * FROM e_nav WHERE portfolio=$1 ORDER BY ts ASC", pid)` |
| `flows{portfolio} @window` | rw | `("SELECT * FROM e_flows WHERE portfolio=$1 AND ts BETWEEN $2 AND $3 ORDER BY ts ASC", pid, w0, w1)` |
| `closures{portfolio} @window` | rw | `("SELECT * FROM e_closures WHERE portfolio=$1 AND ts BETWEEN $2 AND $3 ORDER BY ts ASC", pid, w0, w1)` |
| `cycles{portfolio} @window` | rw | `("SELECT * FROM e_cycles WHERE portfolio=$1 AND ts BETWEEN $2 AND $3 ORDER BY ts ASC", pid, w0, w1)` |
| `events{portfolio} @window` | rw | `("SELECT * FROM e_events WHERE portfolio=$1 AND ts BETWEEN $2 AND $3 ORDER BY ts ASC", pid, w0, w1)` |
| `instrument{portfolio} @asof` | rw | `("SELECT * FROM e_instrument WHERE portfolio=$1 ORDER BY ts ASC", pid)` |
| `instrument{portfolio,instrument} @asof` | rw | `("SELECT * FROM e_instrument WHERE portfolio=$1 AND instrument=$2 ORDER BY ts ASC", pid, iid)` |
| `price{portfolio,instrument} @asof` | rw | `("SELECT * FROM e_price WHERE portfolio=$1 AND instrument=$2 ORDER BY ts ASC", pid, "$benchmark_ids")` |
| `instrument{portfolio} @latest` | rw | `("SELECT DISTINCT ON (portfolio, instrument) * FROM e_instrument WHERE portfolio=$1 ORDER BY portfolio, instrument, ts DESC", pid)` |
| `classification{portfolio} @latest` | pg | `pg("SELECT DISTINCT ON (portfolio, instrument_id) * FROM yfinance.gw_classification WHERE portfolio=$1 ORDER BY portfolio, instrument_id, ts DESC", pid)` |

**Per-panel matcher note:** read each panel's actual selector — if it carries an
`instrument="${instrument_id}"` matcher (several do), use the matcher'd template
and add `iid`. If it carries `@window`, append `w0, w1`. RW selectors use a plain
tuple (auto-routes to RW); `classification` uses `pg(...)` (forced, and also
auto-routes to PG once the view exists).

## Worked example — `total_return.py`

Old:
```python
@bind(nav="nav{portfolio=\"$portfolio_id\"} @asof", flows="flows{portfolio=\"$portfolio_id\"} @window")
@metric(output="scalar")
def total_return(nav, flows): ...
```
New (body unchanged):
```python
@bind(
    nav=("SELECT * FROM e_nav WHERE portfolio=$1 ORDER BY ts ASC", "$portfolio_id"),
    flows=("SELECT * FROM e_flows WHERE portfolio=$1 AND ts BETWEEN $2 AND $3 ORDER BY ts ASC",
           "$portfolio_id", window.t0, window.t1),
)
@metric(output="scalar")
def total_return(nav, flows): ...
```

## Assumptions flagged for human review (user was away during T7)

1. `SELECT *` preserves the old all-columns projection → unchanged panel bodies
   keep working. If any body relied on a projection narrower than the view, this
   over-fetches (harmless) but confirm no body assumes column *position*.
2. `@asof` is unbounded by the window (matches the old compiler). If any panel
   actually wanted "as of window end", it must add `AND ts <= $N` — none observed.
3. `classification` → `yfinance.gw_classification` is lazy-created by yfinance;
   the one classification panel will error until yfinance has initialized its
   schema. Acceptable (same as pre-migration ordering).
