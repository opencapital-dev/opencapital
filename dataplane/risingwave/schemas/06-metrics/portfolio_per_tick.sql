-- ============================================================================
-- portfolio_per_tick — dense per-(portfolio, day) NAV rollup.
-- ----------------------------------------------------------------------------
-- v6 rewrite: DAILY AGGREGATION of the daily per-tick grids.
--
-- The previous v5 re-derived the portfolio from `fold_per_event` over an
-- intraday tick grid (every price tick + every fold business_ts), ASOF-resolving
-- the snapshot, prices and FX per tick. The FX ASOF keyed on (from_ccy, to_ccy)
-- only — every tick fanned out across all pairs sharing a leg (USD→base fan-out
-- > RW's 2048 barrier), amplifying the streaming update and freezing barriers on
-- import (same root cause T3/T4 fixed in instrument_per_tick / cash_per_tick).
--
-- v6 no longer re-derives anything. It is a pure per-(scope, day) AGGREGATION of
-- the now-daily upstream grids — every amplifying ASOF lives (and was removed)
-- upstream, so this MV holds no FX/price/snapshot join at all:
--   * equity columns (equity_value_base, unrealized_equity_*, unrealized_forex_*,
--     instrument_count) = SUM / COUNT over the daily `instrument_per_tick`
--     grouped by (scope_id, event_ts).
--   * cash columns (cash_value_base, unrealized_fx_avg_base, cash_position_count,
--     base_currency fallback) = SUM / COUNT over the daily `cash_per_tick`
--     grouped by (scope_id, event_ts). (Matches v5's cash_per_event branch:
--     realized interest/dividends/fees are sourced from core, not cash.)
--   * portfolio_core rollups (realized_equity_*, realized_forex_*,
--     realized_interest, realized_dividends, fees) are NOT summable from
--     instruments — closed positions are absent from the held grid but still
--     contribute to portfolio_core. They come from the END-OF-DAY snapshot via
--     an equi-join `snapshot_at_day` on (portfolio_id, day) — replacing v5's
--     `ASOF fold_per_ts ON (portfolio_id)` read of `(...).snapshot.portfolio_core`.
--   * nav_base = cash_value_base + equity_value_base; total_gross/net_* use the
--     exact v5 formulas (realized from core + unrealized from the sums - fees).
--
-- DEPENDENCY: this MV assumes `instrument_per_tick` and `cash_per_tick` are the
-- DAILY forms (1 row per (scope, instrument, day) and (scope, currency, day),
-- event_ts = the calendar day cast to timestamptz) and that `snapshot_at_day`
-- holds the end-of-day fold snapshot per (portfolio, day). The per-tick cutover
-- to daily MVs is a prerequisite. The column contract (names + order) matches v5
-- so downstream consumers (entity views) stay unchanged.
--
-- The grid is keyed off the equity aggregation (days with ≥1 held instrument),
-- matching v5 (whose final SELECT is FROM equity_agg). cash and core are
-- LEFT-joined on the day; both are sourced from the same `snapshot_at_day` that
-- fed the held grid, so a snapshot (and thus a core row) exists for every equity
-- day. Daily-close diff vs gold_portfolio_per_tick: the single-currency
-- portfolio matches 0/0; residuals are confined to a multi-currency portfolio
-- and are the inherited T3/T4 densification family (fx_filled gaps on market
-- holidays → NULL non-base legs; gold's last-price-tick vs true end-of-day
-- snapshot timing for late-day cash events and daily-vs-event-time FX;
-- snapshot_at_day keyed on price-days-only → cash-only event days absent).
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS portfolio_per_tick AS
WITH equity_agg AS (
    -- SUM / COUNT equity columns from the daily instrument grid per (scope, day).
    SELECT
        scope_id,
        event_ts,
        SUM(equity_value_base)            AS equity_value_base,
        SUM(unrealized_equity_fifo_base)  AS unrealized_equity_fifo_base,
        SUM(unrealized_equity_avg_base)   AS unrealized_equity_avg_base,
        SUM(unrealized_forex_fifo_base)   AS unrealized_forex_fifo_base,
        SUM(unrealized_forex_avg_base)    AS unrealized_forex_avg_base,
        MAX(base_currency)                AS base_currency,
        COUNT(*)                          AS instrument_count
    FROM instrument_per_tick
    GROUP BY scope_id, event_ts
),
cash_agg AS (
    -- SUM / COUNT cash columns from the daily cash grid per (scope, day).
    -- unrealized_fx_avg_base ≡ 0 in the daily cash MV (the two daily marks
    -- cancel); summed and carried through to match v5's cash_per_event branch.
    SELECT
        scope_id,
        event_ts,
        SUM(cash_value_base)              AS cash_value_base,
        SUM(unrealized_fx_avg_base)       AS unrealized_fx_avg_base,
        MAX(base_currency)                AS base_currency,
        COUNT(*)                          AS cash_position_count
    FROM cash_per_tick
    GROUP BY scope_id, event_ts
),
core_at_day AS (
    -- End-of-day portfolio_core rollups (realized PnL / fees / dividends across
    -- ALL events incl. closed positions). NOT summable from the held grid;
    -- equi-joined from the end-of-day snapshot (replaces v5's ASOF core read).
    SELECT
        s.portfolio_id                       AS scope_id,
        s.day                                AS day,
        (s.snapshot -> 'portfolio_core')     AS core
    FROM snapshot_at_day s
)
SELECT
    'portfolio'                                       AS scope_type,
    eq.scope_id                                       AS scope_id,
    eq.event_ts                                       AS event_ts,
    COALESCE(eq.base_currency, cash.base_currency)    AS base_currency,
    COALESCE(cash.cash_value_base, 0.0)               AS cash_value_base,
    eq.equity_value_base                              AS equity_value_base,
    COALESCE(cash.cash_value_base, 0.0)
        + COALESCE(eq.equity_value_base, 0.0)         AS nav_base,
    -- Realized fields sourced from portfolio_core (includes closed positions,
    -- which are absent from the held instrument grid).
    COALESCE((core.core ->> 'realized_equity_fifo_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_equity_fifo_base,
    COALESCE((core.core ->> 'realized_equity_avg_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_equity_avg_base,
    COALESCE((core.core ->> 'realized_forex_fifo_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_forex_fifo_base,
    COALESCE((core.core ->> 'realized_forex_avg_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_forex_avg_base,
    COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_interest_base,
    COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_dividends_base,
    eq.unrealized_equity_fifo_base                    AS unrealized_equity_fifo_base,
    eq.unrealized_equity_avg_base                     AS unrealized_equity_avg_base,
    eq.unrealized_forex_fifo_base + COALESCE(cash.unrealized_fx_avg_base, 0.0)
                                                      AS unrealized_forex_fifo_base,
    eq.unrealized_forex_avg_base  + COALESCE(cash.unrealized_fx_avg_base, 0.0)
                                                      AS unrealized_forex_avg_base,
    COALESCE((core.core ->> 'fees_base')::DOUBLE PRECISION, 0.0) AS fees_base,
    -- total_*_base: realized (from core) + unrealized (from the sums) + interest
    -- + dividends (gross), minus fees (net). Two cost-basis variants (fifo, avg).
    (COALESCE((core.core ->> 'realized_equity_fifo_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_forex_fifo_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
        + COALESCE(eq.unrealized_equity_fifo_base, 0.0)
        + COALESCE(eq.unrealized_forex_fifo_base, 0.0)
        + COALESCE(cash.unrealized_fx_avg_base, 0.0)) AS total_gross_fifo_base,
    (COALESCE((core.core ->> 'realized_equity_avg_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_forex_avg_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
        + COALESCE(eq.unrealized_equity_avg_base, 0.0)
        + COALESCE(eq.unrealized_forex_avg_base, 0.0)
        + COALESCE(cash.unrealized_fx_avg_base, 0.0)) AS total_gross_avg_base,
    (COALESCE((core.core ->> 'realized_equity_fifo_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_forex_fifo_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
        + COALESCE(eq.unrealized_equity_fifo_base, 0.0)
        + COALESCE(eq.unrealized_forex_fifo_base, 0.0)
        + COALESCE(cash.unrealized_fx_avg_base, 0.0)
        - COALESCE((core.core ->> 'fees_base')::DOUBLE PRECISION, 0.0))              AS total_net_fifo_base,
    (COALESCE((core.core ->> 'realized_equity_avg_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_forex_avg_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
        + COALESCE(eq.unrealized_equity_avg_base, 0.0)
        + COALESCE(eq.unrealized_forex_avg_base, 0.0)
        + COALESCE(cash.unrealized_fx_avg_base, 0.0)
        - COALESCE((core.core ->> 'fees_base')::DOUBLE PRECISION, 0.0))              AS total_net_avg_base,
    eq.instrument_count                               AS instrument_count,
    COALESCE(cash.cash_position_count, 0)             AS cash_position_count
FROM equity_agg eq
LEFT JOIN core_at_day core
    ON  core.scope_id = eq.scope_id
    AND core.day      = eq.event_ts::date
LEFT JOIN cash_agg cash
    ON  cash.scope_id       = eq.scope_id
    AND cash.event_ts::date = eq.event_ts::date;
