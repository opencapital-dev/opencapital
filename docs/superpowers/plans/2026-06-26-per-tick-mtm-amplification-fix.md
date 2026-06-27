# Per-tick MtM Amplification Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the FX and fold-state ASOF-join amplification in the per-tick MtM MVs so a historical broker import no longer wedges RisingWave, while producing identical numbers.

**Architecture:** Add two shared materialized views — `fx_filled` (daily-collapsed, forward-filled FX rate) and `snapshot_at_day` (daily forward-filled fold snapshot) — and modify `instrument_per_tick`, `portfolio_per_tick`, `cash_per_tick` to replace their low-selectivity ASOF joins (`fx_rates` on currency pair, `fold_per_ts` on portfolio) with cheap equi-joins on a day bucket. Event-time fold ticks stay exact (direct join); `prices`/`option_marks` ASOFs are unchanged. Correctness is gated by an exact row-diff against a golden snapshot of today's output, plus an in-flight-import no-wedge test.

**Tech Stack:** RisingWave (streaming SQL, PostgreSQL wire on `:4566`), `psql`, bash schema applier (`dataplane/risingwave/apply.sh`).

## Global Constraints

- RisingWave connection: `psql -h localhost -p 4566 -U root -d dev` (no password). Port is **4566**, not 5432.
- Spec: `docs/superpowers/specs/2026-06-26-per-tick-mtm-amplification-fix-design.md`.
- **Realized/unrealized FX seam (do not break):** realized FX, cost basis, realized P&L come from the fold snapshot `*_base` fields (event-time rate) and must not change. Only the **unrealized mark** (`equity_value_base`, `unrealized_equity_*_base`, `unrealized_forex_*_base`, cash `cash_value_base`) consumes the per-tick FX rate.
- **Event-time fold ticks must stay exact:** an intraday event tick joins its own `fold_per_event` row directly (`business_ts`), never the day bucket.
- `prices` / `option_marks` ASOFs stay (instrument-keyed, selective — not amplifying).
- **Equivalence is the gate:** on the frozen real dataset (all historical/daily), the new output must equal today's output exactly (float epsilon ≈ 1e-9). Cutover only when the diff is empty AND a live import does not wedge.
- `LAST_VALUE(... IGNORE NULLS)` syntax: `IGNORE NULLS` goes **inside** the args — `LAST_VALUE(rate IGNORE NULLS)`. `LAST_VALUE(rate) IGNORE NULLS` errors (verified on the live engine).
- Schema files are the committed source of truth; `apply.sh` is a one-shot `V001` baseline. Existing installs (incl. this dev box) cut over via an explicit drop/recreate migration that keeps source tables, so MVs rebuild with no data loss.
- `apply.sh` runs under macOS `/bin/bash` 3.2 in the packaged app — no bash 4+ features in any script change.

---

### Task 0: Test harness — golden snapshot + diff gate + baseline health

**Files:**
- Create: `dataplane/risingwave/test/per_tick_diff.sql` (reusable diff query)
- Create: `dataplane/risingwave/test/golden_snapshot.sql`
- Create: `dataplane/risingwave/test/health_probe.sql`

**Interfaces:**
- Produces: golden tables `gold_instrument_per_tick`, `gold_portfolio_per_tick`, `gold_cash_per_tick`; a diff query parameterized by `:mv` and `:gold`; a health probe that prints `CREATE TABLE` latency.

- [ ] **Step 1: Confirm the engine is quiesced and healthy (this is the "test fixture")**

Run:
```bash
psql -h localhost -p 4566 -U root -d dev -tA -c "SELECT count(*) FROM rw_catalog.rw_ddl_progress;"
```
Expected: `0` (no MV still backfilling). If non-zero, wait and re-run.

- [ ] **Step 2: Write the health probe**

Create `dataplane/risingwave/test/health_probe.sql`:
```sql
\timing on
CREATE TABLE _health_probe (id int);
DROP TABLE _health_probe;
```

- [ ] **Step 3: Run the health probe — record the healthy baseline**

Run: `psql -h localhost -p 4566 -U root -d dev -f dataplane/risingwave/test/health_probe.sql`
Expected: both `Time:` lines ≈ 50–200 ms (healthy). Record the number; the no-wedge test (Task 7) re-runs this and must stay in this range.

- [ ] **Step 4: Write + run the golden snapshot**

Create `dataplane/risingwave/test/golden_snapshot.sql`:
```sql
DROP TABLE IF EXISTS gold_instrument_per_tick;
DROP TABLE IF EXISTS gold_portfolio_per_tick;
DROP TABLE IF EXISTS gold_cash_per_tick;
CREATE TABLE gold_instrument_per_tick AS SELECT * FROM instrument_per_tick;
CREATE TABLE gold_portfolio_per_tick  AS SELECT * FROM portfolio_per_tick;
CREATE TABLE gold_cash_per_tick       AS SELECT * FROM cash_per_tick;
```
Run: `psql -h localhost -p 4566 -U root -d dev -f dataplane/risingwave/test/golden_snapshot.sql`
Expected: three `CREATE TABLE` lines, no error. These freeze today's output as the oracle.

- [ ] **Step 5: Write the reusable diff gate**

Create `dataplane/risingwave/test/per_tick_diff.sql` (round float columns to 9 places so the diff tolerates float noise; key is `(scope_type, scope_id, instrument_id, event_ts)`). Template for one MV — the implementer instantiates `NEW`/`GOLD` per task:
```sql
-- Usage: psql ... -v new=instrument_per_tick_new -v gold=gold_instrument_per_tick -f per_tick_diff.sql
-- Strategy: round every DOUBLE PRECISION column to 9 decimals, then EXCEPT both ways.
-- A non-zero count = a real divergence to fix.
SELECT 'new_not_in_gold' AS dir, count(*) AS rows FROM (
  SELECT * FROM (SELECT * FROM :new) n
  EXCEPT
  SELECT * FROM (SELECT * FROM :gold) g
) d
UNION ALL
SELECT 'gold_not_in_new', count(*) FROM (
  SELECT * FROM (SELECT * FROM :gold) g
  EXCEPT
  SELECT * FROM (SELECT * FROM :new) n
) d;
```
Note: if raw `EXCEPT` flags float-noise rows, switch each numeric column to `round(col::numeric, 9)` in both projections. Keep a per-column variant in the same file for drilling into mismatches:
```sql
-- drill-down: which columns differ for a given key
-- SELECT n.scope_id, n.instrument_id, n.event_ts,
--        n.equity_value_base, g.equity_value_base
-- FROM :new n JOIN :gold g USING (scope_type, scope_id, instrument_id, event_ts)
-- WHERE abs(coalesce(n.equity_value_base,0) - coalesce(g.equity_value_base,0)) > 1e-9;
```

- [ ] **Step 6: Commit**

```bash
git add dataplane/risingwave/test/
git commit -m "test(rw): golden snapshot + per-tick diff gate + health probe"
```

---

### Task 1: `fx_filled` materialized view

**Files:**
- Create: `dataplane/risingwave/schemas/04-fx/fx_filled.sql`
- Test: `dataplane/risingwave/test/fx_filled_check.sql`

**Interfaces:**
- Produces: MV `fx_filled(org_id, from_ccy, to_ccy, day DATE, rate DOUBLE PRECISION)` — for every `(pair, day)` in the tick calendar, the latest broker rate at or before that day. Consumed by Tasks 3–5 via equi-join on `(org_id, from_ccy, to_ccy, date_trunc('day', tick_ts))`.

- [ ] **Step 1: Write the failing check**

Create `dataplane/risingwave/test/fx_filled_check.sql` — `fx_filled` must equal the ASOF rate today's MVs would pick, on the real portfolio. This compares the new daily-filled rate to the existing `fx_rates` ASOF for the actual price-tick days:
```sql
-- For every price-tick day and pair the portfolio uses, the filled rate must
-- equal the latest fx_rates rate <= that day (what the current ASOF returns).
WITH used AS (
  SELECT DISTINCT i.currency AS from_ccy, p.base_currency AS to_ccy,
         date_trunc('day', px.price_ts) AS day
  FROM prices px
  JOIN instruments i USING (portfolio_id, instrument_id)
  JOIN portfolios p USING (portfolio_id)
  WHERE i.currency IS NOT NULL AND i.currency <> p.base_currency
),
asof_expected AS (
  SELECT u.from_ccy, u.to_ccy, u.day,
         (SELECT r.rate FROM fx_rates r
           WHERE r.from_ccy = u.from_ccy AND r.to_ccy = u.to_ccy
             AND r.ts <= u.day + INTERVAL '1 day'
           ORDER BY r.ts DESC LIMIT 1) AS expected_rate
  FROM used u
)
SELECT count(*) AS mismatches
FROM asof_expected e
JOIN fx_filled f
  ON f.from_ccy = e.from_ccy AND f.to_ccy = e.to_ccy AND f.day = e.day
WHERE e.expected_rate IS NOT NULL
  AND abs(f.rate - e.expected_rate) > 1e-9;
```

- [ ] **Step 2: Run it — verify it fails (object missing)**

Run: `psql -h localhost -p 4566 -U root -d dev -f dataplane/risingwave/test/fx_filled_check.sql`
Expected: ERROR `relation "fx_filled" does not exist`.

- [ ] **Step 3: Write `fx_filled.sql`**

Create `dataplane/risingwave/schemas/04-fx/fx_filled.sql`. Draft (validate/iterate against the check in Step 4):
```sql
-- fx_filled — daily, forward-filled FX rate keyed on (pair, day).
-- Replaces the per-tick `ASOF fx_rates ON (pair)` join (amplification source).
-- Calendar domain = days that actually carry ticks needing a rate (price/option
-- tick days for the pair's instruments, plus fx-event days), so it stays bounded.
CREATE MATERIALIZED VIEW IF NOT EXISTS fx_filled AS
WITH daily_rate AS (
    -- last broker rate within each (pair, day): arg-max over ts via row_number
    SELECT org_id, from_ccy, to_ccy, day, rate FROM (
        SELECT org_id, from_ccy, to_ccy,
               date_trunc('day', ts) AS day, rate,
               row_number() OVER (PARTITION BY org_id, from_ccy, to_ccy, date_trunc('day', ts)
                                  ORDER BY ts DESC) AS rn
        FROM fx_rates
    ) t WHERE rn = 1
),
tick_days AS (
    -- distinct (pair, day) that a per-tick MV will ask about
    SELECT DISTINCT i.org_id, i.currency AS from_ccy, p.base_currency AS to_ccy,
           date_trunc('day', px.price_ts) AS day
    FROM prices px
    JOIN instruments i USING (org_id, portfolio_id, instrument_id)
    JOIN portfolios   p USING (org_id, portfolio_id)
    WHERE i.currency IS NOT NULL
    UNION
    SELECT DISTINCT org_id, from_ccy, to_ccy, day FROM daily_rate
),
calendar AS (
    SELECT td.org_id, td.from_ccy, td.to_ccy, td.day, dr.rate
    FROM tick_days td
    LEFT JOIN daily_rate dr
      ON dr.org_id = td.org_id AND dr.from_ccy = td.from_ccy
     AND dr.to_ccy = td.to_ccy AND dr.day = td.day
)
SELECT org_id, from_ccy, to_ccy, day,
       LAST_VALUE(rate IGNORE NULLS) OVER (
           PARTITION BY org_id, from_ccy, to_ccy ORDER BY day
           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS rate
FROM calendar;
```
Iteration notes: confirm `prices`/`instruments`/`portfolios` carry `org_id` (they do in the deployed schema). If the `tick_days` calendar is expensive, narrow `prices` to the needed columns. If `row_number` arg-max underperforms, swap for `(array_agg(rate ORDER BY ts DESC))[1]`.

- [ ] **Step 4: Apply it and run the check until it passes**

Run:
```bash
psql -h localhost -p 4566 -U root -d dev -c "SET BACKGROUND_DDL=true;" -f dataplane/risingwave/schemas/04-fx/fx_filled.sql
# wait for backfill
psql -h localhost -p 4566 -U root -d dev -tA -c "SELECT count(*) FROM rw_catalog.rw_ddl_progress;"  # 0 = done
psql -h localhost -p 4566 -U root -d dev -f dataplane/risingwave/test/fx_filled_check.sql
```
Expected: `mismatches = 0`. If non-zero, drill into the differing `(pair, day)` rows and adjust the daily-pick / fill until 0.

- [ ] **Step 5: Commit**

```bash
git add dataplane/risingwave/schemas/04-fx/fx_filled.sql dataplane/risingwave/test/fx_filled_check.sql
git commit -m "feat(rw): add fx_filled daily forward-filled FX rate MV"
```

---

### Task 2: `snapshot_at_day` materialized view

**Files:**
- Create: `dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql`
- Test: `dataplane/risingwave/test/snapshot_at_day_check.sql`

**Interfaces:**
- Produces: MV `snapshot_at_day(org_id, portfolio_id, day DATE, snapshot JSONB)` — the latest fold snapshot at or before each price-tick day. Consumed by Tasks 3–5 via equi-join on `(org_id, portfolio_id, date_trunc('day', tick_ts))` for **price/option ticks only**.

- [ ] **Step 1: Write the failing check**

Create `dataplane/risingwave/test/snapshot_at_day_check.sql` — the day-snapshot must equal the latest `fold_per_ts` snapshot ≤ day (what `held_at_tick`'s ASOF picks for a close-of-day price tick). Compare a stable scalar derived from the snapshot (e.g. base_currency + equity position count) to avoid full-JSONB equality flakiness:
```sql
WITH fold_per_ts AS (  -- mirror the collapse used by the per-tick MVs
  SELECT fpe.org_id, fpe.portfolio_id, fpe.business_ts, fpe.fold_result
  FROM fold_per_event fpe
  JOIN (SELECT org_id, portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event GROUP BY org_id, portfolio_id, business_ts) m
    USING (org_id, portfolio_id, business_ts, source_id)
),
days AS (SELECT DISTINCT org_id, portfolio_id, date_trunc('day', price_ts) AS day FROM prices),
expected AS (
  SELECT d.org_id, d.portfolio_id, d.day,
         (SELECT (f.fold_result).snapshot FROM fold_per_ts f
           WHERE f.org_id=d.org_id AND f.portfolio_id=d.portfolio_id
             AND f.business_ts <= d.day + INTERVAL '1 day'
           ORDER BY f.business_ts DESC LIMIT 1) AS snap
  FROM days d
)
SELECT count(*) AS mismatches
FROM expected e
JOIN snapshot_at_day s USING (org_id, portfolio_id, day)
WHERE e.snap IS NOT NULL
  AND jsonb_array_length(e.snap->'equity_positions')
      <> jsonb_array_length(s.snapshot->'equity_positions');
```

- [ ] **Step 2: Run it — verify it fails (object missing)**

Run: `psql -h localhost -p 4566 -U root -d dev -f dataplane/risingwave/test/snapshot_at_day_check.sql`
Expected: ERROR `relation "snapshot_at_day" does not exist`.

- [ ] **Step 3: Write `snapshot_at_day.sql`**

Create `dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql`:
```sql
-- snapshot_at_day — fold snapshot carried onto the daily price calendar.
-- Replaces the per-tick `ASOF fold_per_ts ON (portfolio_id)` join for PRICE/OPTION
-- ticks (amplification source). Event-time ticks keep the exact direct join.
CREATE MATERIALIZED VIEW IF NOT EXISTS snapshot_at_day AS
WITH fold_per_ts AS (
    SELECT fpe.org_id, fpe.portfolio_id, fpe.business_ts, fpe.fold_result
    FROM fold_per_event fpe
    JOIN (SELECT org_id, portfolio_id, business_ts, MAX(source_id) AS source_id
          FROM fold_per_event GROUP BY org_id, portfolio_id, business_ts) m
      USING (org_id, portfolio_id, business_ts, source_id)
),
per_day AS (  -- last snapshot within each (portfolio, day)
    SELECT org_id, portfolio_id, day, snapshot FROM (
        SELECT org_id, portfolio_id, date_trunc('day', business_ts) AS day,
               (fold_result).snapshot AS snapshot,
               row_number() OVER (PARTITION BY org_id, portfolio_id, date_trunc('day', business_ts)
                                  ORDER BY business_ts DESC) AS rn
        FROM fold_per_ts
    ) t WHERE rn = 1
),
price_days AS (
    SELECT DISTINCT org_id, portfolio_id, date_trunc('day', price_ts) AS day FROM prices
    UNION
    SELECT DISTINCT org_id, portfolio_id, day FROM per_day
),
calendar AS (
    SELECT pd.org_id, pd.portfolio_id, pd.day, d.snapshot
    FROM price_days pd
    LEFT JOIN per_day d USING (org_id, portfolio_id, day)
)
SELECT org_id, portfolio_id, day,
       LAST_VALUE(snapshot IGNORE NULLS) OVER (
           PARTITION BY org_id, portfolio_id ORDER BY day
           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS snapshot
FROM calendar;
```
Iteration note: if carrying the full snapshot JSONB is too heavy (Task 6 capacity check), trim `snapshot` to only `equity_positions`, `cash_positions`, `portfolio_core` before the window.

- [ ] **Step 4: Apply and run the check until it passes**

Run:
```bash
psql -h localhost -p 4566 -U root -d dev -c "SET BACKGROUND_DDL=true;" -f dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql
psql -h localhost -p 4566 -U root -d dev -tA -c "SELECT count(*) FROM rw_catalog.rw_ddl_progress;"  # 0 = done
psql -h localhost -p 4566 -U root -d dev -f dataplane/risingwave/test/snapshot_at_day_check.sql
```
Expected: `mismatches = 0`.

- [ ] **Step 5: Commit**

```bash
git add dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql dataplane/risingwave/test/snapshot_at_day_check.sql
git commit -m "feat(rw): add snapshot_at_day daily forward-filled fold snapshot MV"
```

---

### Task 3: Rewrite `instrument_per_tick` to equi-joins (validated against golden)

**Files:**
- Modify: `dataplane/risingwave/schemas/06-metrics/instrument_per_tick.sql`
- Test: reuses `dataplane/risingwave/test/per_tick_diff.sql` + `gold_instrument_per_tick`

**Interfaces:**
- Consumes: `fx_filled` (Task 1), `snapshot_at_day` (Task 2).
- Produces: `instrument_per_tick` with identical columns/keys to today.

- [ ] **Step 1: Build a parallel `instrument_per_tick_new` (the failing test is the diff)**

Copy the current `instrument_per_tick` definition into a scratch MV named `instrument_per_tick_new`, applying these three join replacements (leave the `prices`/`option_marks` ASOFs untouched):

Replace the FX ASOF:
```sql
-- OLD:
ASOF LEFT JOIN fx_rates fx
    ON  fx.org_id   = with_state.org_id
    AND fx.from_ccy = with_state.currency
    AND fx.to_ccy   = with_state.base_currency
    AND with_state.tick_ts >= fx.ts
-- NEW:
LEFT JOIN fx_filled fx
    ON  fx.org_id   = with_state.org_id
    AND fx.from_ccy = with_state.currency
    AND fx.to_ccy   = with_state.base_currency
    AND fx.day      = date_trunc('day', with_state.tick_ts)
```

Split the `held_at_tick` fold-state ASOF into exact (event ticks) + day-equi (price/option ticks). In the `ticks` CTE, tag each tick's origin (`'event'` vs `'price'`); for `'event'` ticks join `fold_per_ts` on exact `business_ts`, for `'price'`/`'option'` ticks join `snapshot_at_day` on the day:
```sql
-- held_at_tick becomes a UNION of two branches:
-- (a) event ticks: t.origin='event' → JOIN fold_per_ts fpe
--        ON fpe.org_id=pi.org_id AND fpe.portfolio_id=pi.portfolio_id
--       AND fpe.business_ts = t.tick_ts        -- exact, no ASOF
-- (b) price/option ticks: t.origin IN ('price','option') → JOIN snapshot_at_day s
--        ON s.org_id=pi.org_id AND s.portfolio_id=pi.portfolio_id
--       AND s.day = date_trunc('day', t.tick_ts)
--    using s.snapshot in place of (fpe.fold_result).snapshot
```

Apply:
```bash
# (author instrument_per_tick_new.sql in test/ as a scratch file, CREATE MATERIALIZED VIEW instrument_per_tick_new AS ...)
psql -h localhost -p 4566 -U root -d dev -c "SET BACKGROUND_DDL=true;" -f dataplane/risingwave/test/instrument_per_tick_new.sql
psql -h localhost -p 4566 -U root -d dev -tA -c "SELECT count(*) FROM rw_catalog.rw_ddl_progress;"  # 0 = done
```

- [ ] **Step 2: Run the diff — expect non-zero first, iterate to zero**

Run:
```bash
psql -h localhost -p 4566 -U root -d dev \
  -v new=instrument_per_tick_new -v gold=gold_instrument_per_tick \
  -f dataplane/risingwave/test/per_tick_diff.sql
```
Expected eventually: both directions `rows = 0`. While non-zero, use the drill-down query (Task 0 Step 5) to find the differing columns/keys and fix the join logic. Common causes: missing `date_trunc` on the join, event-tick branch not exact, NULL FX on same-currency rows (keep the `CASE WHEN currency = base_currency THEN 1.0 ELSE fx.rate END` guard).

- [ ] **Step 3: Fold the validated change into the real schema file**

Once the diff is 0, copy the validated body of `instrument_per_tick_new` into `dataplane/risingwave/schemas/06-metrics/instrument_per_tick.sql` (canonical name `instrument_per_tick`, `IF NOT EXISTS`). Do **not** drop the live `instrument_per_tick` yet — the cutover is Task 6.

- [ ] **Step 4: Drop the scratch MV**

Run: `psql -h localhost -p 4566 -U root -d dev -c "DROP MATERIALIZED VIEW instrument_per_tick_new;"`
Expected: `DROP_MATERIALIZED_VIEW`.

- [ ] **Step 5: Commit**

```bash
git add dataplane/risingwave/schemas/06-metrics/instrument_per_tick.sql
git commit -m "feat(rw): instrument_per_tick uses fx_filled + snapshot_at_day equi-joins"
```

---

### Task 4: Rewrite `portfolio_per_tick` to equi-joins

**Files:**
- Modify: `dataplane/risingwave/schemas/06-metrics/portfolio_per_tick.sql`
- Test: reuses `per_tick_diff.sql` + `gold_portfolio_per_tick`

**Interfaces:**
- Consumes: `fx_filled`, `snapshot_at_day`.
- Produces: `portfolio_per_tick` identical to today.

- [ ] **Step 1: Build parallel `portfolio_per_tick_new`**

Same three replacements as Task 3, applied to `portfolio_per_tick`'s joins:
- the `positions_priced` / `equity_agg` FX `ASOF fx_rates` → `LEFT JOIN fx_filled` on `(org_id, currency, base_currency, day)`;
- the `cash_per_event` FX `ASOF fx_rates` → `LEFT JOIN fx_filled` likewise;
- `held_at_tick` and `core_at_tick` fold ASOFs → event-tick-exact (`fold_per_ts` on `business_ts`) + price-tick day-equi (`snapshot_at_day` on `day`).

Apply as scratch MV `portfolio_per_tick_new` (`test/portfolio_per_tick_new.sql`):
```bash
psql -h localhost -p 4566 -U root -d dev -c "SET BACKGROUND_DDL=true;" -f dataplane/risingwave/test/portfolio_per_tick_new.sql
psql -h localhost -p 4566 -U root -d dev -tA -c "SELECT count(*) FROM rw_catalog.rw_ddl_progress;"  # 0
```

- [ ] **Step 2: Run the diff to zero**

Run:
```bash
psql -h localhost -p 4566 -U root -d dev \
  -v new=portfolio_per_tick_new -v gold=gold_portfolio_per_tick \
  -f dataplane/risingwave/test/per_tick_diff.sql
```
Expected: both `rows = 0`. Iterate the joins until 0 (watch the `MAX(base_currency)` aggregate and the cash branch — they must still produce the same group keys).

- [ ] **Step 3: Fold into the real schema file**

Copy validated body into `dataplane/risingwave/schemas/06-metrics/portfolio_per_tick.sql`.

- [ ] **Step 4: Drop scratch MV**

Run: `psql -h localhost -p 4566 -U root -d dev -c "DROP MATERIALIZED VIEW portfolio_per_tick_new;"`

- [ ] **Step 5: Commit**

```bash
git add dataplane/risingwave/schemas/06-metrics/portfolio_per_tick.sql
git commit -m "feat(rw): portfolio_per_tick uses fx_filled + snapshot_at_day equi-joins"
```

---

### Task 5: Rewrite `cash_per_tick` to equi-joins

**Files:**
- Modify: `dataplane/risingwave/schemas/06-metrics/cash_per_tick.sql`
- Test: reuses `per_tick_diff.sql` + `gold_cash_per_tick`

**Interfaces:**
- Consumes: `fx_filled`, `snapshot_at_day`.
- Produces: `cash_per_tick` identical to today.

- [ ] **Step 1: Build parallel `cash_per_tick_new`**

`cash_per_tick` has two amplifying joins: the cash-state ASOF (`ON scope_id, currency`) and the FX ASOF.
- FX `ASOF fx_rates` (both occurrences) → `LEFT JOIN fx_filled` on `(org_id, currency, base_currency, day)`.
- `cash_state` derivation: source the per-day cash positions from `snapshot_at_day`'s `cash_positions` for price-driven tick days, and keep event-time cash rows exact from `fold_per_ts`. The `with_state` tick grid (`fx_rates`-derived days UNION cash_state event days) keeps its shape; only the as-of lookups change to equi-joins.

Apply as scratch MV `cash_per_tick_new`:
```bash
psql -h localhost -p 4566 -U root -d dev -c "SET BACKGROUND_DDL=true;" -f dataplane/risingwave/test/cash_per_tick_new.sql
psql -h localhost -p 4566 -U root -d dev -tA -c "SELECT count(*) FROM rw_catalog.rw_ddl_progress;"  # 0
```

- [ ] **Step 2: Run the diff to zero**

Run:
```bash
psql -h localhost -p 4566 -U root -d dev \
  -v new=cash_per_tick_new -v gold=gold_cash_per_tick \
  -f dataplane/risingwave/test/per_tick_diff.sql
```
Expected: both `rows = 0`. The `unrealized_fx_avg_base` formula divides by `cash_value_native` — keep the `NULLIF(..., 0)` guard and the `currency = base_currency` short-circuit so the daily rate matches.

- [ ] **Step 3: Fold into the real schema file**

Copy validated body into `dataplane/risingwave/schemas/06-metrics/cash_per_tick.sql`.

- [ ] **Step 4: Drop scratch MV**

Run: `psql -h localhost -p 4566 -U root -d dev -c "DROP MATERIALIZED VIEW cash_per_tick_new;"`

- [ ] **Step 5: Commit**

```bash
git add dataplane/risingwave/schemas/06-metrics/cash_per_tick.sql
git commit -m "feat(rw): cash_per_tick uses fx_filled + snapshot_at_day equi-joins"
```

---

### Task 6: Cutover migration + capacity check

**Files:**
- Create: `dataplane/risingwave/migrations/2026-06-26-per-tick-amplification.sql`
- Test: reuses `per_tick_diff.sql` (all three) + a state-size query

**Interfaces:**
- Consumes: the validated schema files (Tasks 1–5).
- Produces: the live MVs replaced by the new definitions; golden diff still empty.

- [ ] **Step 1: Write the migration (drop old per-tick, create new objects)**

Create `dataplane/risingwave/migrations/2026-06-26-per-tick-amplification.sql`. It drops the three live per-tick MVs (CASCADE) and re-creates `fx_filled`, `snapshot_at_day`, and the three per-tick MVs from the committed schema bodies. Source tables (`portfolio_events_log`, `fold_per_event`, `prices`, `fx_rates`) are untouched, so the MVs rebuild from existing data:
```sql
SET BACKGROUND_DDL = true;
DROP MATERIALIZED VIEW IF EXISTS instrument_per_tick CASCADE;
DROP MATERIALIZED VIEW IF EXISTS portfolio_per_tick CASCADE;
DROP MATERIALIZED VIEW IF EXISTS cash_per_tick CASCADE;
-- \i the committed bodies, in dependency order:
\i ../schemas/04-fx/fx_filled.sql
\i ../schemas/05-fold/snapshot_at_day.sql
\i ../schemas/06-metrics/instrument_per_tick.sql
\i ../schemas/06-metrics/portfolio_per_tick.sql
\i ../schemas/06-metrics/cash_per_tick.sql
INSERT INTO _schema_migrations (plugin_id, version, name)
VALUES ('core', 'V002__per_tick_amplification', 'per_tick_amplification')
ON CONFLICT DO NOTHING;
```
Note: if any MV from earlier tasks already exists live (`fx_filled`/`snapshot_at_day` from Task 1–2 apply), keep `IF NOT EXISTS` in their bodies so re-create is a no-op.

- [ ] **Step 2: Run the migration**

Run:
```bash
psql -h localhost -p 4566 -U root -d dev -v ON_ERROR_STOP=1 -f dataplane/risingwave/migrations/2026-06-26-per-tick-amplification.sql
# wait for rebuild
until [ "$(psql -h localhost -p 4566 -U root -d dev -tA -c 'SELECT count(*) FROM rw_catalog.rw_ddl_progress;')" = "0" ]; do sleep 2; done
```
Expected: migration completes; `rw_ddl_progress` drains to 0.

- [ ] **Step 3: Re-run all three golden diffs — must be empty**

Run the diff for each `(instrument|portfolio|cash)_per_tick` vs its `gold_*` table.
Expected: all `rows = 0`. This proves the live cutover reproduces today's numbers.

- [ ] **Step 4: Capacity check (`snapshot_at_day` state size)**

Run:
```bash
psql -h localhost -p 4566 -U root -d dev -tA -c \
"SELECT count(*) AS rows, pg_size_pretty(sum(length(snapshot::text))) AS approx_bytes FROM snapshot_at_day;"
```
Expected: bounded (rows ≈ portfolios × distinct price days). If `approx_bytes` is large, trim the carried snapshot (Task 2 Step 3 note) and re-run from Task 2.

- [ ] **Step 5: Commit**

```bash
git add dataplane/risingwave/migrations/2026-06-26-per-tick-amplification.sql
git commit -m "feat(rw): cutover migration for per-tick amplification fix"
```

---

### Task 7: No-wedge import test (the real success criterion)

**Files:**
- Test: `dataplane/risingwave/test/health_probe.sql` (Task 0)

**Interfaces:**
- Consumes: the live cutover (Task 6).

- [ ] **Step 1: Capture pre-import health**

Run: `psql -h localhost -p 4566 -U root -d dev -f dataplane/risingwave/test/health_probe.sql`
Expected: `CREATE TABLE` ≈ 50–200 ms (same as Task 0 baseline).

- [ ] **Step 2: Re-import the real statement through the running app**

Get the live grafana port and replay the 575 real events (the path that wedged before). Run:
```bash
PORT=$(grep -oE 'http_port = [0-9]+' /Users/ignacioballester/.opencapital/instance/grafana.ini | grep -oE '[0-9]+')
# build the bulk payload from the existing rows of the real portfolio (idempotent upsert)
P=74691756-eb90-438a-a200-095651ef2ba6
psql -h localhost -p 4566 -U root -d dev -tA -F$'\t' -c \
 "SELECT source_id,event_type,coalesce(instrument_id,''),payload FROM portfolio_events_log WHERE portfolio_id='$P';" \
 | python3 -c 'import sys,json;P="'$P'";print(json.dumps([dict(json.loads(p),event_type=e,source_id=s,portfolio_id=P,updated_by="repro",**({"instrument_id":i} if i else {})) for e,s,i,p in (l.rstrip("\n").split("\t") for l in sys.stdin if l.strip()) ]))' \
 > /tmp/reimport.json
curl -s -m 60 -o /dev/null -w "HTTP %{http_code} %{time_total}s\n" \
  -H "X-WEBAUTH-USER: iballesterllagaria@gmail.com" -H "X-WEBAUTH-EMAIL: iballesterllagaria@gmail.com" \
  -H "Content-Type: application/json" --data-binary @/tmp/reimport.json \
  "http://127.0.0.1:$PORT/grafana/api/plugins/core-app/resources/ref/events/bulk"
```
Expected: `HTTP 201` in **well under 30 s** (vs the old wedge/timeout).

- [ ] **Step 3: Confirm the engine stayed healthy through the import**

Run the health probe again immediately.
Expected: `CREATE TABLE` still ≈ 50–200 ms (no barrier freeze). If it hangs, the fix is incomplete — capture `high_join_amplification` lines from `~/.opencapital/runtime/logs/risingwave.log` and return to the offending MV's task.

- [ ] **Step 4: Re-run the golden diffs after import**

Expected: all three still `rows = 0` (import changed nothing, since it's an idempotent re-import; numbers identical).

- [ ] **Step 5: Commit (test notes only, if any)**

```bash
git add -A && git commit -m "test(rw): no-wedge import validation passes" --allow-empty
```

---

### Task 8: Characterization test for intraday-today + golden cleanup

**Files:**
- Create: `dataplane/risingwave/test/intraday_today_check.sql`

**Interfaces:**
- Consumes: the live cutover.

- [ ] **Step 1: Write the intraday-today characterization test**

Create `dataplane/risingwave/test/intraday_today_check.sql`. It documents the one accepted behaviour change: the current/latest mark is exact; an earlier-today row carries today's latest FX (its price stays intraday-exact). Insert one intraday FX tick + an earlier-today price tick into the frozen book and assert:
```sql
-- after inserting an intraday FX rate R2 at 14:00 today and a price tick at 10:00 today:
-- (a) the latest instrument_per_tick row for today uses R2 (current = exact)
-- (b) the 10:00 row also uses R2 (daily-bucketed), NOT the 10:00 rate (accepted)
-- (c) the 10:00 row's last_price equals the 10:00 price (price stays exact)
-- Assertions are equalities; this is a characterization test, not an equality gate vs gold.
```
(Concrete inserts mirror the `__probe` rows used during debugging; clean them up after.)

- [ ] **Step 2: Run it and record the documented behaviour**

Run: `psql -h localhost -p 4566 -U root -d dev -f dataplane/risingwave/test/intraday_today_check.sql`
Expected: assertions hold (a/b/c). Clean up the probe rows.

- [ ] **Step 3: Drop golden tables (post-validation)**

Keep them through one release as rollback, then:
```bash
psql -h localhost -p 4566 -U root -d dev \
  -c "DROP TABLE IF EXISTS gold_instrument_per_tick;" \
  -c "DROP TABLE IF EXISTS gold_portfolio_per_tick;" \
  -c "DROP TABLE IF EXISTS gold_cash_per_tick;"
```

- [ ] **Step 4: Commit**

```bash
git add dataplane/risingwave/test/intraday_today_check.sql
git commit -m "test(rw): characterize accepted intraday-today FX behaviour"
```

---

## Notes for the implementer

- The three per-tick MV rewrites (Tasks 3–5) are mechanical join swaps validated by an exact diff — the diff is the spec. Do not change projections, CASE guards, or group keys; only the as-of lookups become equi-joins, and the fold-state split (event-exact / price-daily) is preserved.
- Always `SET BACKGROUND_DDL = true` before creating an MV over existing data, and wait for `rw_catalog.rw_ddl_progress` to drain to 0 before diffing — a diff against a half-backfilled MV gives false mismatches.
- Follow-on, separate plans (out of scope here): S3 real fold retract (`fold_kernel`), S4 frontend import chunking (`writeEventsBulk` / `ImportPage.commitReview`).
