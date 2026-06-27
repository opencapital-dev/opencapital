-- ============================================================================
-- option_marks — deduped VIEW over `data_log` filtered to prices.option_mark.
-- ----------------------------------------------------------------------------
-- Statement-sourced option mark stream produced by reference-admin's
-- data_io publisher (see ADR-0019). One row per `Open Positions` option
-- entry on a broker statement upload.
--
-- Lives in core (alongside `prices`) rather than the prices plugin because
-- the dense `metrics_equity_position` MV reads it directly — the MV is
-- core, so its dependencies must apply during Phase A.
--
-- v2 (this file) adds a daily dedup: one row per (portfolio, instrument, day)
-- = the observation with the latest observed_at that day. Today's row
-- re-ranks live as new marks arrive.
--
-- Column contract: (portfolio_id, instrument_id, price_ts, ingest_ts, source,
--                   trace_id, close, currency)
--
-- Payload shape (advisory; documented in
-- infra/risingwave/plugins/prices/payload_schema.json):
--   prices.option_mark: {"close": <double>, "currency": "USD"}
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS option_marks AS
WITH obs AS (
    SELECT
        portfolio_id,
        source_id                                                        AS instrument_id,
        observed_at,
        ingest_ts,
        source,
        trace_id,
        CAST((CAST(payload AS JSONB) ->> 'close') AS DOUBLE PRECISION)   AS close,
        CAST(payload AS JSONB) ->> 'currency'                            AS currency
    FROM data_log
    WHERE source_namespace = 'prices.option_mark'
),
ranked AS (
    SELECT
        portfolio_id,
        instrument_id,
        observed_at,
        ingest_ts,
        source,
        trace_id,
        close,
        currency,
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
    ingest_ts,
    source,
    trace_id,
    close,
    currency
FROM ranked
WHERE rn = 1;
