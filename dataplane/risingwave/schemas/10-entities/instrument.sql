-- irr_annualized is absent from instrument_per_tick; excluded here.
CREATE VIEW IF NOT EXISTS e_instrument AS
SELECT
    scope_id                                          AS portfolio,
    instrument_id                                     AS instrument,
    (extract(epoch from event_ts) * 1000000)::bigint  AS ts,
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
    quantity * avg_cost_avg_base                      AS position_size_base,
    equity_value_base                                 AS current_value,
    unrealized_equity_avg_base                        AS unrealized_pnl
FROM instrument_per_tick;
