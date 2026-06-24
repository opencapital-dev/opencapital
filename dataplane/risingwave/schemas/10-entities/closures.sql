CREATE VIEW IF NOT EXISTS e_closures AS
SELECT
    portfolio_id                                           AS portfolio,
    instrument_id                                          AS instrument,
    (extract(epoch from exit_ts) * 1000000)::bigint        AS ts,
    realized_pnl_base                                      AS realized_pnl,
    extract(epoch from (exit_ts - entry_ts))               AS holding_seconds
FROM closures_per_event;
