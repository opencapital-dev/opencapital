-- ============================================================================
-- prices — deduped VIEW over `data_log` filtered to the prices.* namespaces.
-- ----------------------------------------------------------------------------
-- v1 was an MV doing a 2-way UNION across price_quote / price_ohlcv_bar
-- upsert tables. v6 reads from data_log filtered by source_namespace:
--   * prices.quote   — bid/ask quotes → mid price
--   * prices.ohlcv   — OHLCV bars     → close price
--
-- v7 (this file) adds a daily dedup: one row per (portfolio, instrument, day)
-- = the observation with the latest observed_at that day. Today's row
-- re-ranks live as new quotes arrive (no day-close lag).
--
-- Column contract: (portfolio_id, instrument_id, price_ts, kind, price, currency)
-- Consumers (metrics_* MVs, ASOF joins) depend on this contract; do not reorder.
--
-- Payload JSON shape (advisory; documented in
-- infra/risingwave/plugins/prices/payload_schema.json):
--   prices.quote: {"bid_price": <double>, "ask_price": <double>,
--                  "currency": "USD", "venue": "..."}
--   prices.ohlcv: {"open": <double>, "high": <double>, "low": <double>,
--                  "close": <double>, "volume": <double>, "trade_count": <int>,
--                  "bar_cadence": "1m", "currency": "USD", "venue": "..."}
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS prices AS
WITH obs AS (
    SELECT
        portfolio_id,
        source_id                                                              AS instrument_id,
        observed_at,
        (
            (CAST(payload AS JSONB) ->> 'bid_price')::DOUBLE PRECISION
          + (CAST(payload AS JSONB) ->> 'ask_price')::DOUBLE PRECISION
        ) / 2.0                                                                AS price,
        CAST(payload AS JSONB) ->> 'currency'                                  AS currency,
        'QUOTE'                                                                AS kind
    FROM data_log
    WHERE source_namespace = 'prices.quote'

    UNION ALL

    SELECT
        portfolio_id,
        source_id                                                              AS instrument_id,
        observed_at,
        CAST((CAST(payload AS JSONB) ->> 'close') AS DOUBLE PRECISION)         AS price,
        CAST(payload AS JSONB) ->> 'currency'                                  AS currency,
        'OHLCV_BAR'                                                            AS kind
    FROM data_log
    WHERE source_namespace = 'prices.ohlcv'
),
ranked AS (
    SELECT
        portfolio_id,
        instrument_id,
        observed_at,
        price,
        currency,
        kind,
        row_number() OVER (
            PARTITION BY portfolio_id, instrument_id, date_trunc('day', observed_at)
            ORDER BY observed_at DESC
        ) AS rn
    FROM obs
)
SELECT
    portfolio_id,
    instrument_id,
    observed_at  AS price_ts,
    kind,
    price,
    currency
FROM ranked
WHERE rn = 1;
