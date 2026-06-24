-- ohlcv_coverage: a plugin's own published OHLCV coverage per source.
-- portfolio_id kept alongside portfolio: backfill_worker reads it as data
-- to filter tombstone keys to the correct portfolio.
-- observed_at as bigint µs (TimeCol for @latest = MAX(observed_at) per instrument).
CREATE VIEW ohlcv_coverage AS
SELECT plugin_id,
       portfolio_id                                          AS portfolio,
       portfolio_id                                          AS portfolio_id,
       source_id,
       source_id                                              AS instrument,
       (extract(epoch from observed_at) * 1000000)::bigint   AS observed_at,
       (extract(epoch from observed_at) * 1000000)::bigint   AS ts
FROM data_log
WHERE source_namespace = 'prices.ohlcv';
