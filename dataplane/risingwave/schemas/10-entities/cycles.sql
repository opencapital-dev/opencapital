CREATE VIEW IF NOT EXISTS e_cycles AS
SELECT
    portfolio_id                                           AS portfolio,
    instrument_id                                          AS instrument,
    (extract(epoch from close_ts) * 1000000)::bigint       AS ts,
    pnl_base,
    duration_sec,
    CASE WHEN was_re_entry THEN 1.0 ELSE 0.0 END           AS was_re_entry
FROM cycles_per_event;
