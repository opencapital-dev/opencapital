-- ============================================================================
-- prices plugin — V002: carry tenant scope onto the detail VIEWs.
-- ----------------------------------------------------------------------------
-- V001 projected source_id -> instrument_id but dropped org_id/portfolio_id.
-- data_log carries both (Track 2c: yfinance publishes pricing per-portfolio-
-- per-org), so the detail views must surface them for the read-gateway to
-- scope OHLCV/quote reads by tenant like every other entity. Pure projection
-- change; these are VIEWs with no streaming state, so DROP + CREATE is free.
-- ============================================================================

DROP VIEW IF EXISTS prices_quote;
DROP VIEW IF EXISTS prices_ohlcv;

CREATE VIEW prices_quote AS
    SELECT
        org_id,
        portfolio_id,
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
        org_id,
        portfolio_id,
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
