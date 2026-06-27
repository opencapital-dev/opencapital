# Daily price grid — per-tick MtM simplification + amplification fix

- **Date:** 2026-06-26 (re-scoped 2026-06-27)
- **Status:** Draft for review (rev 3 — daily-grid design)
- **Areas:** `oc-plugin-yfinance-app` (ingestor) + `opencapital/dataplane/risingwave/schemas` (MVs)
- **Related memory:** `fx-rates-asof-join-amplification`, `rw-disk-full-barrier-catastrophe`
- **Supersedes:** rev 1 (hot/cold split) and rev 2 (lean daily-bucket of an intraday grid). Both were invalidated when the live data proved to be intraday throughout — see "Why the earlier revisions failed".

## Problem

Importing a broker statement wedges RisingWave: the bulk write exceeds the 30 s plugin
timeout, the epoch freezes, ingestion backs up. Two root causes, one already fixed:

1. **(fixed) Disk full** — RW couldn't checkpoint → barrier catastrophe. Freeing disk restored it.
2. **(this spec) An intraday price grid.** The live ingestor appends *every* realtime quote
   tick to `data_log` (≈38 k rows/day, 13,994 distinct timestamps on 2026-06-26). The per-tick
   MtM MVs build a tick grid from this and ASOF-join `fold_per_event` (keyed on `portfolio_id`)
   and `fx_rates` (keyed on currency pair) at every tick. On import, those low-selectivity
   ASOFs re-scan the whole intraday grid → `high_join_amplification` (17 k–19 k matched
   rows/update) → memory blowup → barrier freeze.

The grid is intraday only because realtime quotes accumulate unbounded. The product does not
need intraday history.

## Model

**One price datapoint per (instrument, day). Today's datapoint moves in real-time.**
- Past day: the day's close (last observation), frozen.
- Today: the latest quote, updating live as ticks arrive.

This is both the desired product behaviour and the fix: a daily grid is ~8× smaller and makes
the per-tick ASOFs cheap, removing the amplification at the source.

## Goals

- Import does not wedge the engine (health probe `CREATE TABLE` stays ~80 ms throughout).
- Exact MtM at the daily-close granularity for past days; live MtM for today.
- Fewer, simpler MVs; lower actor/state footprint (engine already warns at 1671 actors).
- Realized FX / cost basis unchanged (event-sourced in the fold).

## Non-goals

- **S3 fold retract**, **S4 import chunking** — likely unnecessary once the grid is daily;
  kept as backstops only. Separate work if measurement still shows pressure.

## Design

### 1. Ingestor — live-quote key → per day (`oc-plugin-yfinance-app/pkg/plugin/live.go`)

`data_log` has `PRIMARY KEY (rw_key)`, so an INSERT with an existing key upserts. The OHLCV
backfill already exploits this (`backfill_worker.go`: `rw_key` uses `bar.Date`, one row/day).
The live path does not — `publishTick` puts the tick millisecond in the key, so every tick is
a new row. Change it to the day:

```go
// publishTick: key per (instrument, day) → each tick UPSERTS the day's single row;
// today's row moves live, past days freeze at their last (close) tick.
dayUs := time.UnixMicro(observedAtUs).UTC().Truncate(24 * time.Hour).UnixMicro()
rwKey := datakey.DataKey(s.pluginID, QuoteNamespace, tgt.PortfolioID, tgt.InstrumentID, dayUs)
// row's observed_at stays = observedAtUs (actual tick time) so today's point's timestamp advances
```

Result: live quote writes drop from ~38 k/day to **1 row/instrument/day**, eliminating the
ingestion-barrier churn that drives the wedge. No retention job needed. (Optional later:
throttle upsert frequency to cut same-row changelog churn.)

### 2. `prices` MV → daily dedup (`dataplane/risingwave/schemas/03-unifying-views/prices.sql`)

Collapse the quote+OHLCV union to one row per (portfolio, instrument, day) = latest observation
that day (validated as `prices_daily`, 11,017 rows vs 86,743):

```sql
... row_number() OVER (PARTITION BY portfolio_id, instrument_id, date_trunc('day', observed_at)
                       ORDER BY observed_at DESC) = 1
```

Today's partition re-ranks as quotes arrive → today's row moves live. Same treatment for
`option_marks` (`03-unifying-views/option_marks.sql`) for consistency (currently empty).
Belt-and-suspenders even after the ingestor fix, since a day can have both an OHLCV bar and a
last quote — dedup picks one.

### 3. Per-tick MVs — simplify (`dataplane/risingwave/schemas/06-metrics/`)

On a uniform daily grid:

- **Drop the event-tick branch.** Remove `business_ts` ticks from the grid; the day's mark
  already reflects all that day's events (end-of-day fold). This removes the `origin`-tag /
  exact-event-join logic and dissolves the cross-portfolio shared-instrument event-tick
  divergence. Grid = daily price ticks only.
- **`instrument_per_tick`**: daily price grid → `ASOF fold_per_ts` (latest snapshot ≤ tick) →
  `ASOF prices`/`option_marks` (instrument-keyed) → `ASOF fx_rates` (pair). All ASOFs now over
  the small daily grid; no restructuring needed (exact). `cash_per_tick`: unchanged shape over
  the daily grid.
- **`portfolio_per_tick` → mostly an aggregation.** `equity_value_base` and the equity
  unrealized columns = `SUM` over `instrument_per_tick` per (portfolio, day); `cash_*` from
  `cash_per_tick`; keep a single `fold_per_ts` ASOF only for `portfolio_core` rollups (realized
  P&L, fees, dividends — not summable from instruments). Drops the standalone per-instrument
  re-derivation (the largest MV today). Must still emit the columns the entity views read:
  `nav_base, equity_value_base, cash_value_base, total_gross_avg_base, total_net_avg_base, …`.

### 4. Drop `fx_filled` + `snapshot_at_day`

Built for rev 2 (bucketing an intraday grid). Unnecessary on a daily grid — drop both MVs and
their schema files. (−2 MVs.)

## Behaviour change (intentional)

Past intraday marks collapse to one daily close per instrument. This is the product intent
("keep closing prices for yesterday"), not a regression. So the output does **not** equal the
current intraday output row-for-row — but it must equal it **at each day's close**.

## Validation (the gate)

The current golden snapshot is still the oracle, restricted to daily-close granularity:

1. **Daily-close equivalence:** for each (instrument, past day), the new daily
   `instrument_per_tick` value must equal `gold_instrument_per_tick` at that instrument's
   **last tick of that day** (within ~1e-9). The new model only *drops* the intraday points;
   the surviving daily point must match the old close mark exactly. Same for cash/portfolio.
2. **Portfolio aggregation:** `portfolio_per_tick` daily rows = `SUM(instrument_per_tick) +
   cash_per_tick + fold portfolio_core`, validated against the old `portfolio_per_tick`
   close-of-day rows.
3. **Today-live:** inject a quote for today; assert `prices`/`instrument_per_tick` update the
   single today row (price + timestamp advance), not append.
4. **No-wedge import (success criterion):** re-import the real 575-event statement; HTTP 201
   under 30 s and the health probe stays ~80 ms throughout; grid stays daily.
5. **Grid size:** `prices` ≈ 11 k (was 86 k); per-tick row counts drop accordingly.

## Capacity / efficiency

- **−2 MVs** (`fx_filled`, `snapshot_at_day`); `portfolio_per_tick` shrinks to an aggregation.
- `data_log` quote rows: ~38 k/day → ~1/instrument/day at the source.
- Per-tick grid ~8× smaller; ASOFs cheap; amplification gone.

## Rollout

- **Ingestor** (`oc-plugin-yfinance-app`): ship the `live.go` key change; new live quotes start
  upserting per day. Existing intraday quote rows remain in `data_log` until compacted (the
  `prices` dedup already hides them; optional one-time purge of old `prices.quote` rows).
- **MVs** (`opencapital`): migration drops the three per-tick MVs + `fx_filled` +
  `snapshot_at_day`, rewrites `prices`/`option_marks`/per-tick, recreates. Source tables
  retained → rebuild from existing data, no loss. `apply.sh` baseline files updated for fresh
  installs.

## Risks & open questions

- **Past-day close source:** on days with both an OHLCV bar and quotes, the dedup picks the
  latest `observed_at` (the last quote ≈ close). Decide if the OHLCV official close should win
  instead (order-by preference). Pre-realtime days are OHLCV-only — unaffected.
- **UTC vs market day** for the day-truncation — start with UTC; revisit if session boundaries
  matter.
- **`portfolio_per_tick` aggregation alignment** — daily grid makes per-(portfolio, day) sums
  well-defined (every held instrument has a daily point via ASOF carry-forward); verify no
  gaps for instruments without a bar on a given day.
- **Two-repo change** — ingestor (`oc-plugin-yfinance-app`) and MVs (`opencapital`) ship
  together; the MV dedup is safe before the ingestor change (handles intraday), so order is
  flexible.

## Why the earlier revisions failed (for the record)

- Rev 1/2 assumed historical prices were daily; the live data is intraday on every recent day
  (realtime quotes appended unbounded). Daily-bucketing an intraday grid flattens intraday
  fold/fx → wrong MtM (rev-2 Task 3 diff 5327/910). The fix is to make the *grid* daily at the
  source, not to bucket an intraday grid downstream.

## Affected files

- `oc-plugin-yfinance-app/pkg/plugin/live.go` — live-quote rw_key → day.
- `dataplane/risingwave/schemas/03-unifying-views/prices.sql` — daily dedup.
- `dataplane/risingwave/schemas/03-unifying-views/option_marks.sql` — daily dedup (consistency).
- `dataplane/risingwave/schemas/06-metrics/instrument_per_tick.sql` — daily grid, drop event-ticks.
- `dataplane/risingwave/schemas/06-metrics/cash_per_tick.sql` — daily grid.
- `dataplane/risingwave/schemas/06-metrics/portfolio_per_tick.sql` — aggregation + core ASOF.
- `dataplane/risingwave/schemas/04-fx/fx_filled.sql` — **delete**.
- `dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql` — **delete**.
- `dataplane/risingwave/migrations/` — cutover migration.
- No change to `events.sql`, `fold_per_event.sql`, `fold_kernel.sql`, `fx_rates.sql`.

## Sequencing

1. Ingestor live-quote key → day (+ unit test).
2. `prices`/`option_marks` daily dedup; verify grid ~11 k + today-live.
3. Simplify per-tick MVs; daily-close-equivalence diff vs golden = pass.
4. Cutover migration; no-wedge import test.
5. Drop `fx_filled`/`snapshot_at_day`.
