-- ============================================================================
-- prices plugin — V001: per-namespace detail VIEWs (no actors).
-- ----------------------------------------------------------------------------
-- Core ships a unifying `prices` view (mid for quotes, close for OHLCV) that
-- the dense metric MVs depend on. This plugin adds detail VIEWs for callers
-- who want full namespace shapes (bid/ask separately, full OHLCV columns,
-- venue, bar cadence).
--
-- These are VIEWs not MVs — no streaming state, no actor cost.
-- ============================================================================

CREATE VIEW prices_quote AS
    SELECT
        source_id                                            AS instrument_id,
        observed_at                                          AS price_ts,
        ingest_ts,
        source,
        trace_id,
        (payload::jsonb ->> 'bid_price')::DOUBLE PRECISION   AS bid_price,
        (payload::jsonb ->> 'bid_size')::DOUBLE PRECISION    AS bid_size,
        (payload::jsonb ->> 'ask_price')::DOUBLE PRECISION   AS ask_price,
        (payload::jsonb ->> 'ask_size')::DOUBLE PRECISION    AS ask_size,
        payload::jsonb ->> 'currency'                        AS currency,
        payload::jsonb ->> 'venue'                           AS venue
    FROM data_log
    WHERE source_namespace = 'prices.quote';

CREATE VIEW prices_ohlcv AS
    SELECT
        source_id                                            AS instrument_id,
        observed_at                                          AS price_ts,
        ingest_ts,
        source,
        trace_id,
        (payload::jsonb ->> 'open')::DOUBLE PRECISION        AS open,
        (payload::jsonb ->> 'high')::DOUBLE PRECISION        AS high,
        (payload::jsonb ->> 'low')::DOUBLE PRECISION         AS low,
        (payload::jsonb ->> 'close')::DOUBLE PRECISION       AS close,
        (payload::jsonb ->> 'volume')::DOUBLE PRECISION      AS volume,
        (payload::jsonb ->> 'trade_count')::BIGINT           AS trade_count,
        payload::jsonb ->> 'bar_cadence'                     AS bar_cadence,
        payload::jsonb ->> 'currency'                        AS currency,
        payload::jsonb ->> 'venue'                           AS venue
    FROM data_log
    WHERE source_namespace = 'prices.ohlcv';
