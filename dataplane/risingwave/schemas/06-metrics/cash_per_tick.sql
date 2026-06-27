-- ============================================================================
-- cash_per_tick — daily-close cash, one row per (scope, currency, day).
-- ----------------------------------------------------------------------------
-- v6 rewrite: DAILY GRID via day-keyed EQUI-JOIN (no amplifying ASOF).
--
-- The previous v5 ran over an intraday tick grid (every fx_rates tick UNION
-- every cash event business_ts) and resolved the FX mark with an ASOF JOIN
-- keyed on (from_ccy, to_ccy) only. That ASOF fanned every tick out across all
-- pairs sharing a leg (USD→base fan-out far over RW's 2048 barrier), amplifying
-- the streaming update and freezing the barriers on import.
--
-- v6 collapses the grid to one row per (currency, calendar day) and swaps the
-- amplifying ASOF for a bounded equi-join:
--   * Grid: unnest the END-OF-DAY fold snapshot `snapshot_at_day` directly.
--     Cash persists in every snapshot once deposited, so one row per
--     (portfolio, currency, day) falls straight out of the unnest — no held-set
--     cross join is needed (unlike instrument_per_tick). The day subsumes the
--     old intraday (fx-tick UNION cash-event) grid.
--   * FX: equi-join `fx_filled` on (from_ccy, to_ccy, day) — the daily forward-
--     filled rate — replacing `ASOF fx_rates ON (from_ccy, to_ccy)`. The
--     `CASE WHEN currency = base_currency THEN 1.0 ELSE fx.rate END` guard
--     stays.
--   * The intraday event-time / fx-tick branch is removed entirely.
--
-- cash_value_base is the daily mark (balance × daily FX). It also serves as the
-- cost-basis numerator inside unrealized_fx_avg_base, so the two daily marks
-- cancel and unrealized_fx_avg_base ≡ 0 — matching the gold oracle, whose
-- unrealized_fx_avg_base is identically ~0 (the event-time inner/outer FX marks
-- cancelled there too). The NULLIF(cash_value_native, 0) guard is preserved.
--
-- event_ts output = the calendar `day` (cast to timestamptz at day
-- granularity), consistent with the FX `day` key. The column contract (names +
-- order) matches v5 so downstream consumers stay unchanged.
--
-- NOTE: deposits_cumulative_native / withdrawals_cumulative_native read the
-- `deposits_native` / `withdrawals_native` JSON keys, which the fold snapshot
-- does not emit (the real keys are *_cumulative_native). This reproduces v5's
-- pre-existing behaviour exactly: both columns are NULL in v5, the gold oracle,
-- and here. Left unchanged so the rewrite is a pure grid/FX swap.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS cash_per_tick AS
WITH cash_daily AS (
    -- One row per (portfolio, currency, day): unnest the end-of-day fold
    -- snapshot's cash_positions. The daily snapshot subsumes the old intraday
    -- (fx-tick UNION cash-event) grid.
    SELECT
        s.portfolio_id                                          AS scope_id,
        (cp ->> 'currency')                                     AS currency,
        (s.snapshot -> 'portfolio_core' ->> 'base_currency')    AS base_currency,
        s.day                                                   AS tick_day,
        (cp ->> 'cash_value_native')::DOUBLE PRECISION          AS balance_native,
        (cp ->> 'cash_value_native')::DOUBLE PRECISION          AS cash_value_native,
        (cp ->> 'realized_interest_native')::DOUBLE PRECISION   AS realized_interest_native,
        (cp ->> 'realized_interest_base')::DOUBLE PRECISION     AS realized_interest_base,
        (cp ->> 'realized_dividends_native')::DOUBLE PRECISION  AS realized_dividends_native,
        (cp ->> 'realized_dividends_base')::DOUBLE PRECISION    AS realized_dividends_base,
        (cp ->> 'realized_fx_fifo_base')::DOUBLE PRECISION      AS realized_fx_fifo_base,
        (cp ->> 'realized_fx_avg_base')::DOUBLE PRECISION       AS realized_fx_avg_base,
        (cp ->> 'fees_native')::DOUBLE PRECISION                AS fees_native,
        (cp ->> 'fees_base')::DOUBLE PRECISION                  AS fees_base,
        (cp ->> 'deposits_native')::DOUBLE PRECISION            AS deposits_cumulative_native,
        (cp ->> 'withdrawals_native')::DOUBLE PRECISION         AS withdrawals_cumulative_native
    FROM snapshot_at_day s,
         jsonb_array_elements(s.snapshot -> 'cash_positions') AS cp
),
with_state AS (
    -- Day-keyed equi-join the daily forward-filled FX (replaces the amplifying
    -- ASOF fx_rates ON (from_ccy, to_ccy)).
    SELECT
        c.scope_id,
        c.currency,
        c.base_currency,
        c.tick_day,
        c.balance_native,
        c.cash_value_native,
        c.balance_native
            * (CASE WHEN c.currency = c.base_currency
                    THEN 1.0 ELSE fx.rate END)                 AS cash_value_base,
        fx.rate                                                AS fx_rate,
        c.realized_interest_native,
        c.realized_interest_base,
        c.realized_dividends_native,
        c.realized_dividends_base,
        c.realized_fx_fifo_base,
        c.realized_fx_avg_base,
        c.fees_native,
        c.fees_base,
        c.deposits_cumulative_native,
        c.withdrawals_cumulative_native
    FROM cash_daily c
    LEFT JOIN fx_filled fx
        ON  fx.from_ccy = c.currency
        AND fx.to_ccy   = c.base_currency
        AND fx.day      = c.tick_day
)
SELECT
    'portfolio'                                       AS scope_type,
    with_state.scope_id,
    with_state.currency,
    with_state.base_currency,
    with_state.tick_day::timestamptz                  AS event_ts,
    with_state.balance_native,
    with_state.cash_value_native,
    with_state.cash_value_base,
    with_state.realized_interest_native,
    with_state.realized_interest_base,
    with_state.realized_dividends_native,
    with_state.realized_dividends_base,
    with_state.realized_fx_fifo_base,
    with_state.realized_fx_avg_base,
    with_state.fees_native,
    with_state.fees_base,
    with_state.deposits_cumulative_native,
    with_state.withdrawals_cumulative_native,
    CASE WHEN with_state.currency = with_state.base_currency THEN 0.0
         ELSE with_state.balance_native
              * (with_state.fx_rate
                 - COALESCE(with_state.cash_value_base
                            / NULLIF(with_state.cash_value_native, 0),
                            with_state.fx_rate))
    END                                               AS unrealized_fx_avg_base
FROM with_state
WHERE with_state.balance_native IS NOT NULL;
