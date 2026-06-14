-- Add position_size_base (= quantity * avg_cost_avg_base) to e_instrument.
-- The four native/forex columns (realized_equity_avg_native,
-- realized_forex_avg_base, unrealized_equity_avg_native,
-- unrealized_forex_avg_base) were already present in the view; this migration
-- only adds the one computed column.
-- RisingWave views: DROP + CREATE is safe — no downstream views depend on
-- e_instrument; the gateway is the only reader. Dev data is disposable.

DROP VIEW IF EXISTS e_instrument;

CREATE VIEW e_instrument AS
SELECT
    org_id,
    scope_id                            AS portfolio_id,
    instrument_id,
    event_ts                            AS ts,
    direction,
    quantity,
    currency,
    avg_cost_avg_native,
    avg_cost_avg_base,
    last_price,
    realized_equity_avg_native,
    realized_equity_avg_base,
    realized_forex_avg_base,
    unrealized_equity_avg_native,
    unrealized_equity_avg_base,
    unrealized_forex_avg_base,
    lot_count,
    quantity * avg_cost_avg_base        AS position_size_base
FROM instrument_per_tick;
