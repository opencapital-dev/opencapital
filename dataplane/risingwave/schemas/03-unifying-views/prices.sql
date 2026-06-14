-- ============================================================================
-- prices — VIEW over `data` filtered to the prices.* namespaces (no actors).
-- ----------------------------------------------------------------------------
-- v1 was an MV doing a 2-way UNION across price_quote / price_ohlcv_bar
-- upsert tables. v6 reads from `data.v2` (via data_log) filtered by source_namespace:
--   * prices.quote   — bid/ask quotes → mid price
--   * prices.ohlcv   — OHLCV bars     → close price
--
-- The mid/close projection plus currency stamp matches v1's `prices` MV
-- column contract so the metrics_* MVs (ASOF prices) stay unchanged.
--
-- Payload JSON shape (advisory; documented in
-- infra/risingwave/plugins/prices/payload_schema.json):
--   prices.quote: {"bid_price": <double>, "ask_price": <double>,
--                  "currency": "USD", "venue": "..."}
--   prices.ohlcv: {"open": <double>, "high": <double>, "low": <double>,
--                  "close": <double>, "volume": <double>, "trade_count": <int>,
--                  "bar_cadence": "1m", "currency": "USD", "venue": "..."}
-- ============================================================================

-- v6: org_id propagates so per-tenant logical views can scope downstream
-- reads. Prices land on `data.v2` from the gateway; the ingestor never
-- holds broker creds (ADR-0038).
CREATE MATERIALIZED VIEW prices AS
    SELECT
        org_id,
        portfolio_id,
        source_id AS instrument_id,
        observed_at AS price_ts,
        'QUOTE' AS kind,
        (
            (payload::jsonb ->> 'bid_price')::DOUBLE PRECISION
          + (payload::jsonb ->> 'ask_price')::DOUBLE PRECISION
        ) / 2.0 AS price,
        payload::jsonb ->> 'currency' AS currency
    FROM data_log
    WHERE source_namespace = 'prices.quote'
UNION ALL
    SELECT
        org_id,
        portfolio_id,
        source_id AS instrument_id,
        observed_at AS price_ts,
        'OHLCV_BAR' AS kind,
        (payload::jsonb ->> 'close')::DOUBLE PRECISION AS price,
        payload::jsonb ->> 'currency' AS currency
    FROM data_log
    WHERE source_namespace = 'prices.ohlcv';
