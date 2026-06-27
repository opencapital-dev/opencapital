-- ============================================================================
-- instrument_per_tick — daily-close MtM, one row per (scope, instrument, day).
-- ----------------------------------------------------------------------------
-- v6 rewrite: DAILY GRID via day-keyed EQUI-JOINS (no amplifying ASOFs).
--
-- The previous v5 ran over an intraday tick grid (every price tick + every
-- fold-event business_ts) and resolved fold state / FX with ASOF JOINs. The
-- FX ASOF keyed on (from_ccy, to_ccy) only — every tick fanned out across all
-- pairs sharing the same leg (USD→base fan-out ~9.3k > RW's 2048 barrier),
-- amplifying ~17k rows/update on import and freezing the streaming barriers.
--
-- v6 collapses the grid to one row per (held instrument, calendar day) and
-- swaps the two amplifying ASOFs for bounded equi-joins:
--   * Grid: portfolio_instruments (held set) × portfolio_day_calendar
--     (priced days ∪ fold-event days), joined on portfolio_id. Every active
--     day, every held instrument emits a row — INCLUDING held-but-unpriced
--     days (the instrument's last close is carried forward by the price ASOF).
--   * Fold state: equi-join `snapshot_at_day` on (portfolio_id, day) — the
--     end-of-day fold snapshot — replacing `ASOF fold_per_ts ON (portfolio_id)`.
--   * FX: equi-join `fx_filled` on (from_ccy, to_ccy, day) — daily forward-
--     filled rate — replacing `ASOF fx_rates ON (from_ccy, to_ccy)`.
--   * Price/option: ASOF the daily close keyed on (portfolio, instrument),
--     inequality on price_ts::date — instrument-keyed (selective, NOT
--     amplifying), carrying the last close forward onto unpriced days.
--   * The intraday event-time tick branch is removed entirely.
--
-- DEPENDENCY: this MV assumes `prices` is the DAILY form (1 row per
-- (portfolio, instrument, day); price_ts = the day's last observation). The
-- price ASOF compares on price_ts::date and so is only well-defined when at
-- most one prices row exists per (portfolio, instrument, day). The `prices`
-- cutover to a daily MV is a prerequisite. `option_marks` may remain intraday
-- — the option_daily CTE arg-maxes the last close per (instrument, day).
--
-- event_ts is the calendar `day` (cast to timestamptz at day granularity), so
-- the daily semantics are explicit and the equi-join keys line up. The column
-- contract (names + order) matches v5 so downstream consumers (e_instrument)
-- stay unchanged.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS instrument_per_tick AS
WITH fold_per_ts AS (
    -- One fold row per (portfolio_id, business_ts): the max source_id at that
    -- ts carries the latest cumulative state.
    SELECT fpe.portfolio_id, fpe.business_ts, fpe.instrument_id, fpe.fold_result
    FROM fold_per_event fpe
    JOIN (
        SELECT portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event
        GROUP BY portfolio_id, business_ts
    ) m USING (portfolio_id, business_ts, source_id)
),
portfolio_day_calendar AS (
    -- Days a portfolio is active: union of priced days and fold-event days.
    -- A held-but-unpriced day must still emit a row, so fold days are unioned
    -- in even when no price landed.
    SELECT DISTINCT portfolio_id, price_ts::date AS day
      FROM prices
    UNION
    SELECT DISTINCT portfolio_id, date_trunc('day', business_ts)::date AS day
      FROM fold_per_ts
),
portfolio_instruments AS (
    -- All (portfolio, instrument) pairs fold has ever seen with qty>0.
    SELECT DISTINCT fpe.portfolio_id, (ep ->> 'instrument_id') AS instrument_id
      FROM fold_per_ts fpe,
           jsonb_array_elements((fpe.fold_result).snapshot -> 'equity_positions') AS t(ep)
),
grid AS (
    -- Dense daily grid: every held instrument × every active day for its
    -- portfolio.
    SELECT pi.portfolio_id, pi.instrument_id, cal.day
      FROM portfolio_instruments pi
      JOIN portfolio_day_calendar cal
        ON cal.portfolio_id = pi.portfolio_id
),
held_at_day AS (
    -- Equi-join the end-of-day fold snapshot for (portfolio, day). Replaces
    -- the amplifying ASOF fold_per_ts ON (portfolio_id).
    SELECT
        g.portfolio_id,
        g.instrument_id,
        g.day                        AS tick_ts,
        s.snapshot                   AS snap
    FROM grid g
    LEFT JOIN snapshot_at_day s
        ON  s.portfolio_id = g.portfolio_id
        AND s.day          = g.day
),
prices_keyed AS (
    -- Daily close keyed for an instrument-selective ASOF on day >= price_day.
    SELECT portfolio_id, instrument_id, price_ts::date AS price_day, price
    FROM prices
),
option_daily AS (
    -- Last option close per (portfolio, instrument, day) for the option ASOF.
    SELECT portfolio_id, instrument_id, price_day, close
    FROM (
        SELECT portfolio_id, instrument_id,
               price_ts::date AS price_day, close,
               row_number() OVER (
                   PARTITION BY portfolio_id, instrument_id, price_ts::date
                   ORDER BY price_ts DESC
               ) AS rn
        FROM option_marks
    ) t
    WHERE rn = 1
),
unpacked AS (
    SELECT
        h.portfolio_id                                            AS scope_id,
        h.instrument_id                                           AS instrument_id,
        h.tick_ts                                                 AS tick_ts,
        (ep ->> 'quantity')::DOUBLE PRECISION                     AS quantity,
        (ep ->> 'direction')                                      AS direction,
        (ep ->> 'currency')                                       AS currency,
        (ep ->> 'base_currency')                                  AS base_currency,
        (ep ->> 'lot_count')::INT                                 AS lot_count,
        (ep ->> 'avg_cost_fifo_native')::DOUBLE PRECISION         AS avg_cost_fifo_native,
        (ep ->> 'avg_cost_avg_native')::DOUBLE PRECISION          AS avg_cost_avg_native,
        (ep ->> 'avg_cost_fifo_base')::DOUBLE PRECISION           AS avg_cost_fifo_base,
        (ep ->> 'avg_cost_avg_base')::DOUBLE PRECISION            AS avg_cost_avg_base,
        (ep ->> 'realized_equity_fifo_native')::DOUBLE PRECISION  AS realized_equity_fifo_native,
        (ep ->> 'realized_equity_avg_native')::DOUBLE PRECISION   AS realized_equity_avg_native,
        (ep ->> 'realized_equity_fifo_base')::DOUBLE PRECISION    AS realized_equity_fifo_base,
        (ep ->> 'realized_equity_avg_base')::DOUBLE PRECISION     AS realized_equity_avg_base,
        (ep ->> 'realized_forex_fifo_base')::DOUBLE PRECISION     AS realized_forex_fifo_base,
        (ep ->> 'realized_forex_avg_base')::DOUBLE PRECISION      AS realized_forex_avg_base
    FROM held_at_day h,
         jsonb_array_elements(h.snap -> 'equity_positions') AS t(ep)
    WHERE ep ->> 'instrument_id' = h.instrument_id
      AND (ep ->> 'quantity')::DOUBLE PRECISION > 0
),
with_state AS (
    SELECT
        u.*,
        i.kind                                                    AS kind,
        COALESCE(i.contract_multiplier, 1.0)                      AS contract_multiplier
    FROM unpacked u
    LEFT JOIN instruments i
        ON  i.portfolio_id  = u.scope_id
        AND i.instrument_id = u.instrument_id
)
SELECT
    'portfolio'                                       AS scope_type,
    with_state.scope_id,
    with_state.instrument_id,
    with_state.kind,
    with_state.tick_ts::timestamptz                   AS event_ts,
    with_state.quantity,
    with_state.direction,
    with_state.currency,
    with_state.base_currency,
    with_state.lot_count,
    with_state.avg_cost_fifo_native,
    with_state.avg_cost_avg_native,
    with_state.avg_cost_fifo_base,
    with_state.avg_cost_avg_base,
    with_state.realized_equity_fifo_native,
    with_state.realized_equity_avg_native,
    with_state.realized_equity_fifo_base,
    with_state.realized_equity_avg_base,
    with_state.realized_forex_fifo_base,
    with_state.realized_forex_avg_base,
    CASE WHEN with_state.kind = 'option'
         THEN om.close ELSE px.price END               AS last_price,
    with_state.quantity
        * COALESCE(CASE WHEN with_state.kind = 'option'
                        THEN om.close ELSE px.price END,
                   with_state.avg_cost_avg_native)
        * with_state.contract_multiplier
        * (CASE WHEN with_state.currency = with_state.base_currency
                THEN 1.0 ELSE fx.rate END)            AS equity_value_base,
    with_state.quantity
        * (CASE WHEN with_state.kind = 'option' THEN om.close ELSE px.price END
           - with_state.avg_cost_fifo_native)
        * with_state.contract_multiplier
        * (CASE WHEN with_state.currency = with_state.base_currency
                THEN 1.0 ELSE fx.rate END)            AS unrealized_equity_fifo_base,
    with_state.quantity
        * (CASE WHEN with_state.kind = 'option' THEN om.close ELSE px.price END
           - with_state.avg_cost_avg_native)
        * with_state.contract_multiplier
        * (CASE WHEN with_state.currency = with_state.base_currency
                THEN 1.0 ELSE fx.rate END)            AS unrealized_equity_avg_base,
    with_state.quantity
        * (CASE WHEN with_state.kind = 'option' THEN om.close ELSE px.price END
           - with_state.avg_cost_fifo_native)
        * with_state.contract_multiplier              AS unrealized_equity_fifo_native,
    with_state.quantity
        * (CASE WHEN with_state.kind = 'option' THEN om.close ELSE px.price END
           - with_state.avg_cost_avg_native)
        * with_state.contract_multiplier              AS unrealized_equity_avg_native,
    with_state.quantity
        * (with_state.avg_cost_fifo_native
           * (CASE WHEN with_state.currency = with_state.base_currency
                   THEN 1.0 ELSE fx.rate END)
           - with_state.avg_cost_fifo_base)
        * with_state.contract_multiplier              AS unrealized_forex_fifo_base,
    with_state.quantity
        * (with_state.avg_cost_avg_native
           * (CASE WHEN with_state.currency = with_state.base_currency
                   THEN 1.0 ELSE fx.rate END)
           - with_state.avg_cost_avg_base)
        * with_state.contract_multiplier              AS unrealized_forex_avg_base
FROM with_state
ASOF LEFT JOIN prices_keyed px
    ON  px.portfolio_id  = with_state.scope_id
    AND px.instrument_id = with_state.instrument_id
    AND with_state.tick_ts >= px.price_day
ASOF LEFT JOIN option_daily om
    ON  om.portfolio_id  = with_state.scope_id
    AND om.instrument_id = with_state.instrument_id
    AND with_state.tick_ts >= om.price_day
LEFT JOIN fx_filled fx
    ON  fx.from_ccy = with_state.currency
    AND fx.to_ccy   = with_state.base_currency
    AND fx.day      = with_state.tick_ts;
