-- ============================================================================
-- instrument_per_tick — dense per-(scope, instrument, tick_ts) MtM.
-- ----------------------------------------------------------------------------
-- v5 rewrite: held-at-tick semantics. Reads `fold_per_event` directly as
-- the source of truth (no more snapshots durable table or
-- instrument_per_event cross-section). For every price tick, ASOFs the
-- latest fold_per_event row for the portfolio at-or-before tick_ts, and
-- lateral-unnests its equity_positions array filtered to the tick's
-- instrument_id. A closed instrument is absent from equity_positions
-- (v5 fold drops qty<=0 entries) → no row materialises → no phantom.
--
-- Multiple fold_per_event rows can share a business_ts (multiple events
-- at the same ts). The OverWindow operator orders within a business_ts
-- by source_id, so the row with the largest source_id carries the
-- latest cumulative state. ASOF picks that row.
--
-- Column contract matches the previous v4 instrument_per_tick output so
-- portfolio_per_tick and downstream consumers stay unchanged.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS instrument_per_tick AS
WITH fold_per_ts AS (
    -- One fold row per (portfolio_id, business_ts): max source_id
    -- at that ts carries the latest cumulative state. Prevents ASOF +
    -- lateral from double-counting when multiple events share a
    -- business_ts.
    SELECT fpe.portfolio_id, fpe.business_ts, fpe.instrument_id, fpe.fold_result
    FROM fold_per_event fpe
    JOIN (
        SELECT portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event
        GROUP BY portfolio_id, business_ts
    ) m USING (portfolio_id, business_ts, source_id)
),
ticks AS (
    -- Prices / option_marks from their unifying MVs;
    -- the fold-driven branch carries portfolio_id from fold_per_ts.
    SELECT instrument_id, price_ts AS tick_ts FROM prices
    UNION ALL
    SELECT instrument_id, price_ts AS tick_ts FROM option_marks
    UNION ALL
    -- Event-time ticks: every fold_per_event row that names an instrument
    -- gives that instrument a fresh state row, so per-trade MtM rows show
    -- up in instrument_per_tick at the trade business_ts.
    SELECT DISTINCT instrument_id, business_ts AS tick_ts
      FROM fold_per_ts
     WHERE instrument_id IS NOT NULL
),
portfolio_instruments AS (
    -- All (portfolio, instrument) pairs that fold has ever seen with qty>0.
    SELECT DISTINCT fpe.portfolio_id, (ep ->> 'instrument_id') AS instrument_id
      FROM fold_per_ts fpe,
           jsonb_array_elements((fpe.fold_result).snapshot -> 'equity_positions') AS t(ep)
),
held_at_tick AS (
    SELECT
        pi.portfolio_id,
        pi.instrument_id,
        t.tick_ts,
        (fpe.fold_result).snapshot AS snap
    FROM portfolio_instruments pi
    JOIN ticks t
        ON  t.instrument_id = pi.instrument_id
    ASOF LEFT JOIN fold_per_ts fpe
        ON  fpe.portfolio_id = pi.portfolio_id
        AND t.tick_ts >= fpe.business_ts
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
    FROM held_at_tick h,
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
    with_state.tick_ts                                AS event_ts,
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
ASOF LEFT JOIN prices px
    ON  px.portfolio_id  = with_state.scope_id
    AND px.instrument_id = with_state.instrument_id
    AND with_state.tick_ts >= px.price_ts
ASOF LEFT JOIN option_marks om
    ON  om.portfolio_id  = with_state.scope_id
    AND om.instrument_id = with_state.instrument_id
    AND with_state.tick_ts >= om.price_ts
ASOF LEFT JOIN fx_rates fx
    ON  fx.from_ccy = with_state.currency
    AND fx.to_ccy   = with_state.base_currency
    AND with_state.tick_ts >= fx.ts;
