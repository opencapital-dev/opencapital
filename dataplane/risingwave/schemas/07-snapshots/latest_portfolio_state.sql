-- ============================================================================
-- latest_portfolio_state — latest snapshot per portfolio, marked to "now".
-- ----------------------------------------------------------------------------
-- v5 rewire: reads `fold_per_event` directly (no snapshots durable table).
-- One row per portfolio carrying the most recent snapshot's payload + a
-- query-time `current_horizon_ts` from the prices stream.
--
-- Kept as a VIEW (not MV) so `current_horizon_ts` is evaluated on demand —
-- the scalar subquery on `prices` cannot be materialised in a streaming MV
-- without nested-loop joins.
-- ============================================================================

CREATE VIEW latest_portfolio_state AS
    SELECT
        fpe.portfolio_id,
        fpe.business_ts                                       AS as_of_ts,
        (fpe.fold_result).snapshot -> 'equity_positions'      AS equity_positions,
        (fpe.fold_result).snapshot -> 'cash_positions'        AS cash_positions,
        (fpe.fold_result).snapshot -> 'portfolio_core'        AS portfolio_core,
        (SELECT MAX(price_ts) FROM prices)                    AS current_horizon_ts
    FROM fold_per_event fpe
    JOIN (
        SELECT portfolio_id, MAX(business_ts) AS business_ts
        FROM fold_per_event
        GROUP BY portfolio_id
    ) m USING (portfolio_id, business_ts)
    JOIN (
        -- Within the latest business_ts, pick the row with the largest
        -- source_id (the OverWindow operator's stable ordering within a
        -- business_ts, carrying the latest cumulative state).
        SELECT portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event
        GROUP BY portfolio_id, business_ts
    ) ms USING (portfolio_id, business_ts, source_id);
