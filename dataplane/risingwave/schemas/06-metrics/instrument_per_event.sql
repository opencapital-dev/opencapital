-- ============================================================================
-- instrument_per_event — sparse per-(scope, instrument, event_ts) projection.
-- ----------------------------------------------------------------------------
-- v5 simplification: reads `fold_per_event` directly. The v4 chain
-- (fold MV → sinks → snapshots table → instrument_per_event) is collapsed
-- to (fold_per_event → instrument_per_event). RW persists MVs to
-- Hummock/S3 already; no durability is lost.
--
-- One row per (scope_id, instrument_id, business_ts, source_id). The
-- source_id is included in the grain so multiple events at the same
-- business_ts each produce a row (the OverWindow operator orders them
-- by source_id within a business_ts, so the row with the largest
-- source_id carries the latest cumulative state).
--
-- v5 fold filters qty<=0 out of `equity_positions` at the closing
-- event, so closed positions are naturally absent from this MV after
-- closure. No phantom-position propagation.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS instrument_per_event AS
WITH fold_per_ts AS (
    -- Collapse to one fold_per_event row per (portfolio_id,
    -- business_ts) by picking the largest source_id at that business_ts.
    -- The OverWindow operator orders within a business_ts by source_id, so
    -- the max-source_id row carries the latest cumulative state.
    SELECT fpe.portfolio_id, fpe.business_ts, fpe.source_id, fpe.fold_result
    FROM fold_per_event fpe
    JOIN (
        SELECT portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event
        GROUP BY portfolio_id, business_ts
    ) m USING (portfolio_id, business_ts, source_id)
),
unpacked AS (
    SELECT
        fpe.portfolio_id                                          AS scope_id,
        (ep ->> 'instrument_id')                                  AS instrument_id,
        fpe.business_ts                                           AS event_ts,
        fpe.source_id                                             AS source_id,
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
    FROM fold_per_ts fpe,
         jsonb_array_elements((fpe.fold_result).snapshot -> 'equity_positions') AS t(ep)
)
SELECT
    u.scope_id,
    u.instrument_id,
    u.event_ts,
    u.source_id,
    i.kind                                AS kind,
    COALESCE(i.contract_multiplier, 1.0)  AS contract_multiplier,
    u.quantity, u.direction, u.currency, u.base_currency, u.lot_count,
    u.avg_cost_fifo_native, u.avg_cost_avg_native,
    u.avg_cost_fifo_base,   u.avg_cost_avg_base,
    u.realized_equity_fifo_native, u.realized_equity_avg_native,
    u.realized_equity_fifo_base,   u.realized_equity_avg_base,
    u.realized_forex_fifo_base,    u.realized_forex_avg_base
FROM unpacked u
LEFT JOIN instruments i
    ON  i.portfolio_id  = u.scope_id
    AND i.instrument_id = u.instrument_id;
