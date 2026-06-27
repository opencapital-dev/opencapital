# Per-tick MtM amplification fix — design (lean)

- **Date:** 2026-06-26
- **Status:** Draft for review
- **Area:** RisingWave compute pipeline (`opencapital/dataplane/risingwave/schemas/`)
- **Related memory:** `fx-rates-asof-join-amplification`, `rw-disk-full-barrier-catastrophe`

## Problem

Importing a broker statement into a portfolio that already has price history wedges
RisingWave: a bulk `POST /ref/events/bulk` exceeds the 30 s plugin-call timeout, the epoch
freezes, and ingestion itself backs up (a RW table write flows through the same barrier
machinery, so a frozen epoch blocks the INSERT, not just the downstream MVs).

Root cause is **join amplification** in the per-tick mark-to-market MVs
(`instrument_per_tick`, `portfolio_per_tick`, `cash_per_tick`). Their as-of joins are keyed
on low-cardinality columns, so every row of a portfolio piles into one hash bucket:

| Join | Keyed on | Right side updates when | Observed `matched_rows_len` |
|------|----------|-------------------------|-----------------------------|
| FX mark (4 sites) | `(from_ccy, to_ccy)` — currency pair | a TRADE/DIVIDEND/FX event adds an `fx_rates` row | ~17,028 |
| Fold state (`held_at_tick`, cash_state) | `(org_id, portfolio_id[, currency])` | any event changes `fold_per_event` | ~19,183 |

A new right-side row at time `R` re-evaluates every left tick with `tick_ts >= R`. A bulk
historical import inserts many right-side rows deep in the past, each re-deriving a long
suffix of the grid: ≈ (events × forward ticks) re-evaluations in one barrier → memory spike
(~3.7 GB) → barrier can't complete → wedge. The `prices` / `option_marks` ASOFs are keyed on
`instrument_id` (selective) and are **not** affected.

Confirmed empirically: 575 synthetic trades with fake tickers import in 0.12 s; the same 575
real events take 13.4 s and wedge a busy engine; a freshly-recovered engine re-wedged
immediately after one real re-import, logging both amplification signatures above.

## Goals

- Bulk historical imports complete without wedging the engine.
- Real-time **current** mark-to-market stays exact (latest price × latest FX × latest state).
- Historical series and the current value are **identical to today** (validated by an
  equivalence gate).
- Realized FX / cost basis remain event-sourced and untouched.
- **Minimise MV count and chain depth** — RisingWave already warns
  `actor count 1335 exceeds recommended`; every MV and every hop is real capacity.

## Non-goals (separate follow-on specs)

- **Fold retract (S3).** `fold_per_event` is a cumulative window aggregate; importing a
  historical event still re-folds its tail because the sentinel `retract` forces a full
  re-accumulate. This design bounds the *per-tick fan-out*, not the fold replay. A real
  reverse-plan retract is tracked separately (high correctness risk; needs an
  `accumulate∘retract == identity` property harness).
- **Frontend import chunking (S4).** Split `writeEventsBulk` into ~50–100-event batches so
  each request stays under the timeout regardless of MV cost. Low-risk, independent.

## Key invariant: realized vs unrealized FX

Preserve the existing seam:

- **Realized FX, cost basis, realized P&L** are computed in the fold at event time from each
  trade's own broker rate (`events.event_time_fx = COALESCE(payload->>'fx_rate_to_base',
  fx_rates ASOF)`), baked into the snapshot `*_base` fields. These never touch the per-tick
  grid and do not change.
- **Unrealized mark** of open positions (`equity_value_base`, `unrealized_equity_*_base`,
  `unrealized_forex_*_base`, cash `cash_value_base`) uses the per-tick `fx.rate`. This — and
  only this — is what we re-key.

## Design (lean): two shared MVs + in-place equi-join swap

No hot/cold split, no compatibility views. We add **two shared materialized MVs** and modify
the three existing per-tick MVs in place to consume them via equi-joins. Public names,
columns, and keys are unchanged.

### New MV 1 — `fx_filled(org_id, pair, day, rate)`

One MV: collapse `fx_rates` to a daily rate, then forward-fill across the calendar.

```sql
-- inside one CREATE MATERIALIZED VIEW:
WITH per_day AS (
  SELECT org_id, from_ccy, to_ccy, date_trunc('day', ts) AS day,
         -- last rate in the day (max ts), via arg-max or a windowed pick
         ...
  FROM fx_rates
  GROUP BY org_id, from_ccy, to_ccy, date_trunc('day', ts)
)
SELECT org_id, from_ccy, to_ccy, day,
       LAST_VALUE(rate IGNORE NULLS) OVER (
         PARTITION BY org_id, from_ccy, to_ccy ORDER BY day
         ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS rate
FROM (per_day left-joined onto the dense day calendar for the pair)
```

Shared by all four FX-marking consumers (`events` fallback optional + 3 per-tick).
**Verified on the live engine:** `LAST_VALUE(rate IGNORE NULLS) OVER (…)` creates as a
streaming MV (note: `IGNORE NULLS` goes *inside* the args; `LAST_VALUE(v) IGNORE NULLS`
errors). The "dense day calendar" domain is bounded to the days that actually carry ticks
needing a rate (distinct tick-days per pair), not an open-ended range.

### New MV 2 — `snapshot_at_day(org_id, portfolio_id, day, snapshot)`

The fold snapshot carried onto the daily calendar: forward-fill `fold_per_ts` per portfolio
(`LAST_VALUE(... IGNORE NULLS) OVER (PARTITION BY org_id, portfolio_id ORDER BY day)`). One
MV serves all three per-tick MVs (it holds the full snapshot — equity *and* cash positions).
A new event re-touches only days ≥ its `business_ts` day. **Trim** the carried payload to the
fields the per-tick MVs actually unnest, to bound state (see Capacity).

### In-place changes to the three per-tick MVs

For each, replace the amplifying as-of joins; keep everything else:

- **FX mark:** `ASOF fx_rates ON (pair) …` → `equi-join fx_filled ON (org_id, pair, date_trunc('day', tick_ts))`.
- **Fold state, price/option ticks:** `ASOF fold_per_ts ON (portfolio_id) …` → `equi-join snapshot_at_day ON (org_id, portfolio_id, date_trunc('day', tick_ts))` (price/option ticks are daily closes, so end-of-day is the correct as-of).
- **Fold state, event-time ticks:** keep **exact** — join `fold_per_ts` directly on `(org_id, portfolio_id, business_ts)`; the tick *is* that fold row, so no ASOF, no fan-out. (Required: an intraday event tick on a multi-event day must use the as-of-*that-event* snapshot, not end-of-day.)
- **`prices` / `option_marks` ASOFs:** unchanged (instrument-keyed, selective).
- **`cash_per_tick`:** same treatment — FX via `fx_filled`; its cash-state as-of via `snapshot_at_day` cash positions (day-keyed for price-driven ticks, exact for event ticks).

Effect: an imported historical event re-derives only the affected day buckets through cheap
hash-equi-join probes (fan-out ≈ instruments on those days); the low-selectivity ASOF bucket
re-scan is gone. Real-time edge updates touch only today's bucket.

## Behaviour & "same data as today"

- **Historical rows (before today):** identical. Historical price/FX data is daily, so a
  `(pair, day)` equi-join returns exactly what the per-tick ASOF returns — one value per day.
  Daily bucketing is **lossless** on daily data. Event-time fold rows stay exact (direct join).
- **Current value (now):** exact. Today's bucket is "today's latest rate / latest snapshot so
  far" and updates live; the current mark is latest-price (ASOF, intraday-exact) × latest-FX ×
  latest-state.
- **Intraday history *within today*:** **flattened** — a 10:00 row carries today's latest FX
  and latest fold state rather than the 10:00 values (its *price* stays intraday-exact). This
  is the one deliberate behaviour change vs today, and it only manifests when intraday data
  exists within the current day. Accepted: we want live current value + daily history, not an
  intraday-FX-accurate replay of earlier today.

If an intraday-FX-accurate curve of the current day later becomes a hard requirement, the
fallback is the hot/cold split (below) — but it is not built now.

## Capacity / efficiency

- **Net MVs: +2** (`fx_filled`, `snapshot_at_day`), modifying three existing MVs in place. No
  hot/cold doubling, no views.
- **Chain depth: +1 hop** on two paths (`fx_rates → fx_filled → per_tick`,
  `fold_per_ts → snapshot_at_day → per_tick`).
- **Removes 6 amplifying ASOF operators** (4 FX + 2 fold) in favour of two shared MVs +
  equi-joins. The two new MVs are computed **once** and fanned out, where the old ASOFs each
  held large per-key join state inside every consumer — so total state should **drop**.
- **Watch `snapshot_at_day` size:** it stores one (trimmed) snapshot per portfolio per day.
  Trim to unnested fields; confirm it stays bounded as portfolios/history grow. This is the
  main capacity item to measure.

## Equivalence test (cutover gate)

1. **Freeze** the real 575-event portfolio (real currency mix that triggers the
   amplification), no live feed, let RisingWave quiesce (no pending barriers).
2. **Golden snapshot** current outputs: `CREATE TABLE gold_<name> AS SELECT * FROM <name>;`
   for the three per-tick MVs.
3. **Build the new MVs** (`fx_filled`, `snapshot_at_day`) and a *parallel* copy of each
   per-tick MV under a temp name using the new joins — does not disturb the live MVs.
4. **Diff** on `(scope_type, scope_id, instrument_id, event_ts)`: `EXCEPT` both directions
   empty, and a full-outer-join shows every numeric column within ~1e-9 relative. Because the
   frozen dataset is all historical/daily, this must be **exact** — that is the gate.
5. **Intraday-today behaviour test:** inject an intraday FX tick + an earlier-today price tick;
   assert the documented behaviour (current row exact; earlier-today row carries latest FX,
   exact price). This is a *characterization* test of the accepted change, not an equality gate.
6. **In-flight import test:** re-run a full import against the new MVs; confirm the diff vs
   golden stays empty after convergence **and** the engine does not wedge — `CREATE TABLE
   x(id int)` stays ~80 ms throughout, bulk `events/bulk` returns under the timeout.

Cutover (point the public names at the new definitions) only when 4 and 6 pass. Keep golden
tables for one release as rollback.

## Rollout / migration

- New migration version under `dataplane/risingwave/schemas/` (idempotent; `apply.sh` tracks
  it in `_schema_migrations`). RW cannot `ALTER` an MV, so the migration `DROP … CASCADE`s the
  three per-tick MVs and re-creates them on top of the two new intermediates.
- Applying it on existing data rebuilds the MVs from the event log — a one-time backfill,
  exactly the path the in-flight import test exercises.

## Risks & open questions

- **`snapshot_at_day` state size** — the main capacity risk; trim the carried payload and
  measure on real data before cutover.
- **`fx_filled` daily-pick** — "last rate in the day" needs an arg-max over `ts` per
  `(pair, day)`; confirm the chosen formulation (windowed pick vs `max(ts)` self-join) is
  incremental and cheap.
- **`events` fallback ASOF** — `events.event_time_fx` also ASOFs `fx_rates` on the pair key,
  but is bounded by event count and short-circuits to the payload rate when present. Left
  as-is; can point it at `fx_filled` later if measurement says it matters.
- **Intraday-today flattening** — confirmed acceptable (see Behaviour); the characterization
  test pins it so a future change is deliberate.

## Documented fallback (not built now): hot/cold split

If intraday-FX-accurate marks *within the current day* become required: keep `*_per_tick` as
the day-bucketed `*_cold` (this design), add a small `*_hot` MV that is the current per-tick
logic with exact ASOFs filtered to `tick_ts >= date_trunc('day', NOW())`, and union them
behind the public view. The `NOW()` boundary is **verified working** on the live engine
(`WHERE ts >= date_trunc('day', NOW())` creates as a temporal-filter MV). This adds MVs, so it
is held in reserve.

## Affected files

- `dataplane/risingwave/schemas/04-fx/` — add `fx_filled` (and keep `fx_rates`).
- `dataplane/risingwave/schemas/05-fold/` or `06-metrics/` — add `snapshot_at_day`.
- `dataplane/risingwave/schemas/06-metrics/instrument_per_tick.sql` — swap amplifying ASOFs for equi-joins.
- `dataplane/risingwave/schemas/06-metrics/portfolio_per_tick.sql` — same.
- `dataplane/risingwave/schemas/06-metrics/cash_per_tick.sql` — same.
- `dataplane/risingwave/apply.sh` / `_schema_migrations` — new migration version.
- No change to `04b-events/events.sql`, `05-fold/fold_per_event.sql`,
  `03-functions/fold_kernel.sql` (the latter two are the S3 retract scope), or
  `oc-plugin-core-app/dashboards/metrics-instrument.json` (contract preserved).

## Sequencing

1. This spec (lean amplification fix) + S4 import chunking → imports survive, numbers identical.
2. Measure remaining import cost; if the fold replay still dominates, do S3 (real retract).
