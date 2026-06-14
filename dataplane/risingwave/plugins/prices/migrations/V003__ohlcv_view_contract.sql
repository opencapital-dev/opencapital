-- Normalize prices_ohlcv to the view-as-contract shape:
--   portfolio  from portfolio_id (canonical scope alias)
--   instrument from source_id (matches catalog label instrument=instrument_id)
--   ts         bigint µs from observed_at (TimeCol)
-- Multi-value series keeps friendly column names (open/high/low/close/volume/
-- trade_count/bar_cadence/currency/venue) — no single value alias needed.
-- prices_quote is not a catalog entity; left untouched here.

DROP VIEW IF EXISTS prices_ohlcv;

CREATE VIEW prices_ohlcv AS
    SELECT
        org_id,
        portfolio_id                                              AS portfolio,
        source_id                                                 AS instrument,
        (extract(epoch from observed_at) * 1000000)::bigint       AS ts,
        ingest_ts,
        source,
        trace_id,
        (payload::jsonb ->> 'open')::DOUBLE PRECISION             AS open,
        (payload::jsonb ->> 'high')::DOUBLE PRECISION             AS high,
        (payload::jsonb ->> 'low')::DOUBLE PRECISION              AS low,
        (payload::jsonb ->> 'close')::DOUBLE PRECISION            AS close,
        (payload::jsonb ->> 'volume')::DOUBLE PRECISION           AS volume,
        (payload::jsonb ->> 'trade_count')::BIGINT                AS trade_count,
        payload::jsonb ->> 'bar_cadence'                          AS bar_cadence,
        payload::jsonb ->> 'bar_cadence'                          AS cadence,
        payload::jsonb ->> 'currency'                             AS currency,
        payload::jsonb ->> 'venue'                                AS venue
    FROM data_log
    WHERE source_namespace = 'prices.ohlcv';
