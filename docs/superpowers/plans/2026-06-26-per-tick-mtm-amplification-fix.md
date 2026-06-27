# Daily Price Grid — Per-tick MtM Simplification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Collapse the price grid to one datapoint per (instrument, day) with today live, so the per-tick MtM MVs stop amplifying on import — fixed at the source (ingestor) + simplified MVs, with exact daily-close numbers.

**Architecture:** (1) Live ingestor upserts one `data_log` quote row per (instrument, day) instead of appending every tick. (2) `prices`/`option_marks` MVs dedup to 1/(instrument, day), today live. (3) The three per-tick MVs simplify over the daily grid (drop event-time ticks; `portfolio_per_tick` becomes an aggregation). (4) Drop `fx_filled`/`snapshot_at_day`. Correctness gate: new daily output equals the golden snapshot **at each day's last tick** (~1e-6).

**Tech Stack:** Go (yfinance ingestor), RisingWave streaming SQL (`psql -h localhost -p 4566 -U root -d dev`).

## Global Constraints

- Spec: `docs/superpowers/specs/2026-06-26-per-tick-mtm-amplification-fix-design.md` (rev 3, daily-grid).
- Two repos: ingestor `/Users/ignacioballester/trading-code/oc-plugin-yfinance-app`; MVs `/Users/ignacioballester/trading-code/opencapital/.worktrees/per-tick-amplification`.
- **Deployed RW schema is org_id-FREE.** No org_id anywhere.
- RW gotchas (verified): `CREATE MATERIALIZED VIEW` / `CREATE TABLE AS` are ASYNC — after create, poll `SELECT count(*) FROM rw_catalog.rw_ddl_progress` until `0` before reading/diffing. `LAST_VALUE(x IGNORE NULLS)` (IGNORE NULLS inside args). `SET BACKGROUND_DDL=true;` before CREATE over existing data. RW `\d` errors (collation) — use `SELECT * FROM x LIMIT 1`.
- `data_log` has `PRIMARY KEY (rw_key)` → INSERT upserts. `datakey.DataKey(plugin, ns, portfolio, source_id, observed_at)` = `plugin|ns|portfolio|source_id|observed_at`.
- Golden tables exist (taken over the current intraday grid): `gold_instrument_per_tick` (47402), `gold_portfolio_per_tick` (40223), `gold_cash_per_tick` (3418). Health baseline `CREATE TABLE x(id int)` ≈ 35–80 ms.
- Validated artifact: scratch MV `prices_daily` already exists (1/instr/day, 11017 rows) — Task 2 promotes its SQL.
- Numbers change intentionally: past intraday marks collapse to one daily close. The gate is **daily-close equivalence** (new daily point == old golden at that day's last tick), NOT full row equality.
- Commits: `--no-gpg-sign` fallback AUTHORIZED if 1Password signing fails.
- Realized FX / cost basis (`*_base` from the fold) must not change.

---

### Task 1: Ingestor — live-quote rw_key per day (`oc-plugin-yfinance-app`)

**Files:**
- Modify: `oc-plugin-yfinance-app/pkg/plugin/live.go` (`publishTick`)
- Test: `oc-plugin-yfinance-app/pkg/plugin/live_test.go` (or `live_units_test.go`)

**Interfaces:**
- Produces: live quotes write 1 `data_log` row per (instrument, day); row's `observed_at` = actual tick time; rw_key uses the day.

- [ ] **Step 1: Failing test** — assert the live quote rw_key is day-truncated. Add to the live test file: build two ticks for the same instrument/portfolio on the same UTC day at different times; assert they produce the **same** rw_key (so they upsert), and ticks on different days produce different keys. Use `datakey.DataKey` with the day-truncated micros as the expectation.

- [ ] **Step 2: Run it — fails** (current code uses tick micros → different keys). `cd oc-plugin-yfinance-app && go test ./pkg/plugin/ -run TestLiveQuote -v` → FAIL.

- [ ] **Step 3: Implement** — in `publishTick`, compute the day bucket and use it in the key (keep `observed_at` = actual tick time in the INSERT):
```go
dayUs := time.UnixMicro(observedAtUs).UTC().Truncate(24 * time.Hour).UnixMicro()
rwKey := datakey.DataKey(s.pluginID, QuoteNamespace, tgt.PortfolioID, tgt.InstrumentID, dayUs)
```
(The INSERT keeps `to_timestamp(observedAtUs/1e6)` for `observed_at`.)

- [ ] **Step 4: Run tests** — `go test ./pkg/plugin/ -run TestLiveQuote -v` PASS; then `go test ./pkg/plugin/...` (full package) green.

- [ ] **Step 5: Commit** — `cd oc-plugin-yfinance-app && git add pkg/plugin/live.go pkg/plugin/*_test.go && git commit -m "fix(live): upsert one quote per (instrument, day); today moves live"` (this repo's own branch; --no-gpg-sign if signing fails).

---

### Task 2: `prices` + `option_marks` daily dedup (`opencapital`)

**Files:**
- Modify: `dataplane/risingwave/schemas/03-unifying-views/prices.sql`
- Modify: `dataplane/risingwave/schemas/03-unifying-views/option_marks.sql`
- Test: `dataplane/risingwave/test/prices_daily_check.sql`

**Interfaces:**
- Produces: `prices(portfolio_id, instrument_id, price_ts, price, currency)` = 1 row per (portfolio, instrument, day) = latest obs; today re-ranks live. (Drops the `kind` column — confirmed unused by consumers.) `option_marks` same shape, deduped.

- [ ] **Step 1: Write the check** `dataplane/risingwave/test/prices_daily_check.sql`: assert max rows per (portfolio, instrument, day) = 1, and prices total ≈ 11k (not 86k):
```sql
SELECT (SELECT max(c) FROM (SELECT count(*) c FROM prices_candidate GROUP BY portfolio_id, instrument_id, price_ts::date) z) AS max_per_day,
       (SELECT count(*) FROM prices_candidate) AS total;
```

- [ ] **Step 2: Promote the validated `prices_daily` SQL into the schema file.** Put the deduped body (the `row_number() … =1` over quote∪ohlcv, already validated as scratch MV `prices_daily`) into `prices.sql` under the canonical name `prices` (`CREATE MATERIALIZED VIEW IF NOT EXISTS prices`). Apply the same `row_number()=1` dedup pattern to `option_marks.sql` (partition by portfolio, instrument, day; order observed_at desc).

- [ ] **Step 3: Validate as a scratch MV first** — `CREATE MATERIALIZED VIEW prices_candidate AS <new prices body>`, `SET BACKGROUND_DDL=true`, poll rw_ddl_progress to 0, run the check. Expect `max_per_day=1`, `total≈11017`. Drop `prices_candidate` after.

- [ ] **Step 4: Today-live check** — `SELECT count(*) FROM prices_candidate WHERE price_ts::date = (SELECT max(price_ts::date) FROM prices_candidate);` returns ~ (#instruments) (one row per instrument for the latest day). Record.

- [ ] **Step 5: Commit** the two schema files + check. (Do NOT cut over live `prices` yet — Task 7.)

---

### Task 3: `instrument_per_tick` — daily grid, drop event-ticks (`opencapital`)

**Files:** Modify `dataplane/risingwave/schemas/06-metrics/instrument_per_tick.sql`; Test reuses golden + a daily-close diff.

**Interfaces:** Consumes `prices_daily` (Task 2 candidate) + `fold_per_event` + `fx_rates`. Produces `instrument_per_tick` = 1 row/(instrument, day), exact daily-close MtM.

- [ ] **Step 1: Daily-close diff query** `dataplane/risingwave/test/daily_close_diff.sql` (parameterized `-v new=… -v gold=…`):
```sql
WITH gold_close AS (
  SELECT *, row_number() OVER (PARTITION BY scope_id, instrument_id, event_ts::date ORDER BY event_ts DESC) rn
  FROM :gold )
SELECT count(*) AS mismatches FROM (SELECT * FROM gold_close WHERE rn=1) g
JOIN :new n ON n.scope_id=g.scope_id AND coalesce(n.instrument_id,'')=coalesce(g.instrument_id,'') AND n.event_ts::date=g.event_ts::date
WHERE abs(coalesce(n.equity_value_base,0)-coalesce(g.equity_value_base,0)) > 1e-6
   OR abs(coalesce(n.unrealized_equity_fifo_base,0)-coalesce(g.unrealized_equity_fifo_base,0)) > 1e-6
   OR abs(coalesce(n.last_price,0)-coalesce(g.last_price,0)) > 1e-6;
-- also assert no gold-close key missing from new:
```
(Add the reverse: every gold daily-close key present in `new`.)

- [ ] **Step 2: Build `instrument_per_tick_new` over `prices_daily`** — copy the deployed `instrument_per_tick` body; (a) `FROM prices` → `FROM prices_daily` (both the ticks source and the price ASOF); (b) **remove the event-time ticks branch** of the `ticks` CTE (keep only price/option ticks); keep the fold ASOF and fx ASOF as-is (now over the daily grid). `SET BACKGROUND_DDL=true`, apply, poll to 0.

- [ ] **Step 3: Run the daily-close diff** `-v new=instrument_per_tick_new -v gold=gold_instrument_per_tick` → iterate to `mismatches=0` and no missing keys. Drill into any diffs.

- [ ] **Step 4:** fold the validated body into `instrument_per_tick.sql` (canonical name, `FROM prices`), drop `instrument_per_tick_new`.

- [ ] **Step 5: Commit.**

---

### Task 4: `cash_per_tick` — daily grid (`opencapital`)

**Files:** Modify `dataplane/risingwave/schemas/06-metrics/cash_per_tick.sql`; reuses `daily_close_diff.sql` + `gold_cash_per_tick`.

- [ ] **Step 1: Build `cash_per_tick_new`** over `prices_daily`/`fx_rates`/`fold` daily grid; drop event-time ticks where present (cash grid derives from fx_rates + cash_state — keep daily). Apply, poll to 0.
- [ ] **Step 2: Daily-close diff** vs `gold_cash_per_tick` → `mismatches=0` (cash key has no instrument_id — adjust the diff join to `(scope_id, day)`). Iterate.
- [ ] **Step 3:** fold into `cash_per_tick.sql`; drop scratch.
- [ ] **Step 4: Commit.**

---

### Task 5: `portfolio_per_tick` → aggregation (`opencapital`)

**Files:** Modify `dataplane/risingwave/schemas/06-metrics/portfolio_per_tick.sql`; reuses diff + `gold_portfolio_per_tick`.

**Interfaces:** Consumes `instrument_per_tick` (Task 3) + `cash_per_tick` (Task 4) + `fold_per_event` (for `portfolio_core`). Produces `portfolio_per_tick` with the columns the entity views read: `nav_base, equity_value_base, cash_value_base, total_gross_avg_base, total_net_avg_base`, plus the realized breakdowns.

- [ ] **Step 1: Build `portfolio_per_tick_new`** = per (scope_id, day): `SUM` equity columns from `instrument_per_tick`, cash columns from `cash_per_tick`, and `portfolio_core` rollups (realized P&L/fees/dividends) from a single `ASOF fold_per_ts` (daily). Compute `nav_base = equity + cash`, totals as today. Apply over the daily-built instrument/cash (use the Task 3/4 scratch or the candidate chain), poll to 0.
- [ ] **Step 2: Daily-close diff** vs `gold_portfolio_per_tick` on `(scope_id, day)` for `nav_base, equity_value_base, cash_value_base, total_gross_avg_base, total_net_avg_base` → `mismatches=0`. Iterate (watch the core rollups — they come from fold, not the sum).
- [ ] **Step 3:** fold into `portfolio_per_tick.sql`; drop scratch.
- [ ] **Step 4: Commit.**

---

### Task 6: Delete `fx_filled` + `snapshot_at_day` (`opencapital`)

- [ ] **Step 1:** `git rm dataplane/risingwave/schemas/04-fx/fx_filled.sql dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql` and their test files (`fx_filled_check.sql`, `snapshot_at_day_check.sql`).
- [ ] **Step 2:** Drop from the engine: `psql … -c "DROP MATERIALIZED VIEW IF EXISTS snapshot_at_day; DROP MATERIALIZED VIEW IF EXISTS fx_filled;"` (snapshot_at_day first if it depends on fx_filled — it doesn't; order free).
- [ ] **Step 3: Commit.**

---

### Task 7: Cutover migration + no-wedge import (`opencapital`) — GATED

**Files:** Create `dataplane/risingwave/migrations/2026-06-27-daily-price-grid.sql`.

- [ ] **Step 1: Write the migration** — drop the three per-tick MVs + `prices`/`option_marks` CASCADE, recreate from the updated schema files in dependency order (prices/option_marks → instrument_per_tick, cash_per_tick → portfolio_per_tick), retaining source tables; record `_schema_migrations` version `V002__daily_price_grid`.
- [ ] **Step 2: Pre-cutover health** — `CREATE TABLE x(id int); DROP` ≈ baseline.
- [ ] **Step 3: Run migration**, poll rw_ddl_progress to 0.
- [ ] **Step 4: Post-cutover daily-close diff** — re-run the daily-close diffs vs all three golden tables → `mismatches=0`. Grid: `SELECT count(*) FROM prices` ≈ 11k; per-tick counts dropped.
- [ ] **Step 5: No-wedge import** — replay the 575 real events through grafana (port from `~/.opencapital/instance/grafana.ini`); assert HTTP 201 < 30 s and health probe stays ~80 ms throughout; daily-close diffs still 0.
- [ ] **Step 6: Commit** the migration + updated `apply.sh` baseline note. Drop golden tables after one release.

---

## Notes for the implementer
- Build all candidates as scratch MVs reading `prices_daily` (the validated daily prices) so Tasks 3–5 validate without disturbing the live `prices`; Task 7 does the single atomic cutover.
- Always `SET BACKGROUND_DDL=true` and wait for `rw_ddl_progress=0` before diffing — a diff against a half-backfilled MV is a false mismatch.
- The diff is daily-close-restricted (new vs gold at each day's last tick) — NOT full row equality (intraday past rows intentionally vanish).
