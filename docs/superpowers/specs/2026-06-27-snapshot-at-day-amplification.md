# snapshot_at_day amplification on import — issue + tradeoffs

- **Date:** 2026-06-27
- **Status:** Open issue (fix deferred). Spec only. Root cause is a **leading hypothesis, not yet verified** — see "Verification status".
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

The equi-key is `portfolio_id` only. On an import, every event mutates `fold_per_event` → `fold_per_ts`; each right-side change re-evaluates the matched left rows. The inequality `day + 1 ≥ business_ts` matches **all daily rows on or after that event** — for the larger book (74691756) that's up to **2,515 price-days** (the ASOF left side), against **574 distinct fold timestamps** (the right side) that all change together on import. The matched-row count per barrier therefore far exceeds RisingWave's 2,048 amplification threshold. The per-tick MVs (T3–T5) consume `snapshot_at_day` via cheap day-keyed equi-joins, so they no longer amplify — but this upstream ASOF, in their fold-state *input*, still does. *(Measured 2026-06-27; earlier drafts cited a ~6,095 fan-out — that figure was not reproducible against the daily grid and has been corrected.)*

## Verification status (systematic-debugging)

What is **confirmed**:
- `snapshot_at_day` is a portfolio-keyed ASOF (deployed definition read from `rw_catalog`, matches the snippet above).
- It is a low-selectivity ASOF whose matched set (≤2,515 days × 574 right rows) exceeds the 2,048 threshold — i.e. it **is** an amplifier by construction.
- `fold_per_event` is a cumulative OverWindow (`fold_kernel(enriched) OVER (PARTITION BY portfolio_id ORDER BY business_ts, source_id ROWS UNBOUNDED PRECEDING AND CURRENT ROW)`). A bulk import inserts ~575 events into one partition; an UNBOUNDED-PRECEDING window recomputes the partition tail on each mid-history insert → up to O(575²) ≈ 330 k `fold_kernel` evaluations. This is the deferred **S3**, and it is **upstream** of `snapshot_at_day` — it produces the simultaneous 574-row right-side churn that the ASOF then amplifies. The two are **coupled**.

What is **NOT verified**:
- That `snapshot_at_day`'s ASOF (and not the `fold_per_event` re-accumulation) is the **dominant** cost of the observed async wedge. The wedge log (`wait_for_epoch table_id: 869`) was **rotated away by the RW restart** and `table_id 869` was never mapped to a relation name, so there is no direct evidence naming the frozen executor. Both suspects amplify; their relative cost is unmeasured.
- That the proposed forward-fill rewrite actually clears the wedge. If the fold OverWindow dominates, fixing `snapshot_at_day` alone leaves the import still churning.

**Implication:** run the decisive diagnostic below **before** committing to the forward-fill rewrite, so the fix targets the dominant amplifier rather than the most visible one.

## Why it's an ASOF (the tradeoff already made)

`snapshot_at_day` was originally written as a forward-fill (`LAST_VALUE(snapshot IGNORE NULLS) OVER (PARTITION BY portfolio ORDER BY day)`), which is non-amplifying. That version **exploded the actor count** (1,671 actors, barriers froze) because it referenced `fold_per_event` — itself an OverWindow MV — **twice** through a UNION (once for the snapshots, once to derive the calendar days). The agent fell back to the portfolio-keyed ASOF, which builds cheaply but amplifies on incremental updates. So today's `snapshot_at_day` trades import-time amplification for build-time stability.

## Options + tradeoffs

1. **Forward-fill from a single materialized `fold_per_ts` (recommended).** Materialize the `fold_per_ts` collapse (one row per portfolio/business_ts, the MAX-source_id pick) as its own MV, then build `snapshot_at_day` by forward-filling over the daily calendar referencing that intermediate **once** — avoiding the double-reference of `fold_per_event` that caused the actor explosion.
   - *Why it should be cheaper than the ASOF:* a `LAST_VALUE(snapshot IGNORE NULLS) OVER (PARTITION BY portfolio ORDER BY day)` over a calendar carrying the snapshot on fold-days (NULL elsewhere) recomputes, on a fold-day change, only the filled rows **up to the next fold-day** — bounded by the inter-event gap (~2,515 days / 574 events ≈ 4–5 days avg for the big book), because the next non-null overrides downstream. The ASOF, by contrast, counts the whole inequality-match set (all days-after) toward join amplification. So the asymmetry is gap-sized OverWindow recompute vs all-days-after join match — *not* "bounded vs whole bucket" per se.
   - *Cost:* +1 intermediate MV; forward-filling a large JSONB snapshot per portfolio per day (state ~21 MB seen) — measure actor count + state before/after.
   - *Risk (unvalidated):* the actor explosion could recur even with the single-ref; the snapshot payload is large and forward-filling it across 2,515 days/portfolio inflates window state. Gate on an actor-count + barrier-health check. Combine with option 2 to shrink the payload before betting on this.

2. **Trim the carried payload.** Forward-fill only the fields the per-tick MVs unnest (`equity_positions`, `cash_positions`, `portfolio_core`) instead of the whole snapshot, shrinking the window state and the explosion risk. Combine with (1).

3. **Keep the ASOF; accept import-needs-restart.** Do nothing in the MV; document that a large import async-wedges the engine and a restart clears it. Cheapest, but not production-clean — every bulk import needs an operator restart.

4. **Real fold retract / import chunking (S3) — possibly co-primary, not just complementary.** `fold_per_event`'s UNBOUNDED-PRECEDING OverWindow re-accumulates the partition tail on each mid-history insert (~O(575²) ≈ 330 k `fold_kernel` calls for the big book on a bulk import). This both (a) is its own heavy churn and (b) generates the simultaneous 574-row right-side change that `snapshot_at_day`'s ASOF amplifies — so it may be the **dominant** cause, not a side issue. A reverse-plan retract or import chunking reduces the fold replay. High correctness risk. The diagnostic above decides whether this is required *before* or *instead of* option 1.

## Decisive diagnostic (run before fixing — gated, isolated)

The fix should not start until we know which suspect dominates. The wedge log is gone, so reproduce it on an **isolated** engine (a throwaway data plane or off-hours — **not** the live app the user is about to test/release):

1. Import the 575-event statement; while it propagates, sample which fragment backpressures — RW's `high_join_amplification` WARN names the executor, and `rw_catalog` fragment/actor backpressure metrics show where barrier latency accrues.
2. Counterfactual A: replace `snapshot_at_day`'s ASOF with the forward-fill (option 1) only; re-import; did the wedge clear?
3. Counterfactual B: leave `snapshot_at_day` as-is, reduce the fold churn (S3 reverse-plan retract, or import chunking); re-import; did it clear?

Whichever counterfactual clears the wedge is the dominant cause. If only B clears it, the forward-fill rewrite (option 1) is wasted effort and S3 is the real fix.

## Recommendation

Pending the diagnostic, the **leading** fix is option **1 (+2)**: rebuild `snapshot_at_day` as a forward-fill over a materialized single-reference `fold_per_ts`, trimmed to the unnested fields. But because `fold_per_event`'s OverWindow re-accumulation (S3) is coupled and upstream, **do not assume option 1 alone clears the import wedge** — confirm with the diagnostic first. Validate any fix with: (a) actor count stays bounded after create, (b) `CREATE TABLE x(id int)` stays ~80 ms *during and after* a 575-event import (no async wedge), (c) daily-close diff vs golden unchanged.

## Affected files

- `dataplane/risingwave/schemas/05-fold/snapshot_at_day.sql` — rewrite (forward-fill, single-ref, trimmed).
- possibly add `dataplane/risingwave/schemas/05-fold/fold_per_ts.sql` — materialize the collapse once.
- No change to the per-tick MVs (they already equi-join `snapshot_at_day`).

## Validation gate (when implemented)

Re-run the no-wedge import test: import the 575-event statement, then `CREATE TABLE x(id int)` must stay ~80 ms with no `wait_for_epoch`/`high_join_amplification` in the RW log, and no restart required. Daily-close diff vs golden must be unchanged from the cutover baseline (7 price/qty + 68 equity intended residuals).
