# snapshot_at_day amplification on import — issue + tradeoffs

- **Date:** 2026-06-27
- **Status:** Open issue (fix deferred). Spec only.
- **Area:** `opencapital/dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql`
- **Related:** `fx-rates-asof-join-amplification`, the daily-grid cutover (`2026-06-26-per-tick-mtm-amplification-fix-design.md`).

## Problem

After the daily-grid cutover, the bulk import POST is fast (HTTP 201 in **0.285 s**, was a 30 s timeout) — the *synchronous* wedge is fixed. But the import still **asynchronously** wedges the streaming engine: after the POST returns, the fold's incremental changes propagate and barriers freeze (`wait_for_epoch … elapsed=240 s`), requiring a RisingWave restart to clear (a from-scratch rebuild is a batch ASOF with no incremental amplification, so recovery comes back clean).

The remaining amplifier is **`snapshot_at_day`**. It carries the fold snapshot onto the daily calendar via a **portfolio-keyed ASOF**:

```sql
... ASOF LEFT JOIN fold_per_ts fpe
      ON fpe.portfolio_id = s.portfolio_id
     AND s.day + INTERVAL '1 day' >= fpe.business_ts
```

The equi-key is `portfolio_id` only. On an import, every event mutates `fold_per_event` → `fold_per_ts`; each right-side change re-evaluates **all** of that portfolio's daily rows (~6,095 fan-out for the larger book — over RisingWave's 2,048 amplification threshold). The per-tick MVs (T3–T5) consume `snapshot_at_day` via cheap day-keyed equi-joins, so they no longer amplify — but this upstream ASOF, in their fold-state *input*, still does.

## Why it's an ASOF (the tradeoff already made)

`snapshot_at_day` was originally written as a forward-fill (`LAST_VALUE(snapshot IGNORE NULLS) OVER (PARTITION BY portfolio ORDER BY day)`), which is non-amplifying. That version **exploded the actor count** (1,671 actors, barriers froze) because it referenced `fold_per_event` — itself an OverWindow MV — **twice** through a UNION (once for the snapshots, once to derive the calendar days). The agent fell back to the portfolio-keyed ASOF, which builds cheaply but amplifies on incremental updates. So today's `snapshot_at_day` trades import-time amplification for build-time stability.

## Options + tradeoffs

1. **Forward-fill from a single materialized `fold_per_ts` (recommended).** Materialize the `fold_per_ts` collapse (one row per portfolio/business_ts, the MAX-source_id pick) as its own MV, then build `snapshot_at_day` by forward-filling over the daily calendar referencing that intermediate **once** — avoiding the double-reference of `fold_per_event` that caused the actor explosion. A fold change then recomputes only days-after-change within one pair-partition (bounded), not the whole portfolio bucket.
   - *Cost:* +1 intermediate MV; forward-filling a large JSONB snapshot per portfolio per day (state ~21 MB seen) — measure actor count + state before/after; the OverWindow still recomputes the partition tail on a mid-history insert (inherent, but cheap per row).
   - *Risk:* the actor explosion could recur if the single-ref still fans out; gate on an actor-count + barrier-health check.

2. **Trim the carried payload.** Forward-fill only the fields the per-tick MVs unnest (`equity_positions`, `cash_positions`, `portfolio_core`) instead of the whole snapshot, shrinking the window state and the explosion risk. Combine with (1).

3. **Keep the ASOF; accept import-needs-restart.** Do nothing in the MV; document that a large import async-wedges the engine and a restart clears it. Cheapest, but not production-clean — every bulk import needs an operator restart.

4. **Real fold retract (S3), separately.** The fold's sentinel `retract` forces a full re-accumulate per changed event (O(n²) over an import), which also feeds the post-import churn. A reverse-plan retract reduces the fold replay but does **not** address `snapshot_at_day`'s ASOF directly. High correctness risk; complementary, not a substitute.

## Recommendation

Option **1 (+2)**: rebuild `snapshot_at_day` as a forward-fill over a materialized single-reference `fold_per_ts`, trimmed to the unnested fields. Validate with: (a) actor count stays bounded after create, (b) `CREATE TABLE x(id int)` stays ~80 ms *during and after* a 575-event import (no async wedge), (c) daily-close diff vs golden unchanged. Measure whether the fold replay (S3) still dominates after this; only then consider S3.

## Affected files

- `dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql` — rewrite (forward-fill, single-ref, trimmed).
- possibly add `dataplane/risingwave/schemas/05-fold/fold_per_ts.sql` — materialize the collapse once.
- No change to the per-tick MVs (they already equi-join `snapshot_at_day`).

## Validation gate (when implemented)

Re-run the no-wedge import test: import the 575-event statement, then `CREATE TABLE x(id int)` must stay ~80 ms with no `wait_for_epoch`/`high_join_amplification` in the RW log, and no restart required. Daily-close diff vs golden must be unchanged from the cutover baseline (7 price/qty + 68 equity intended residuals).
