-- ============================================================================
-- option_marks — VIEW over `data` filtered to the prices.option_mark namespace.
-- ----------------------------------------------------------------------------
-- Statement-sourced option mark stream produced by reference-admin's
-- data_io publisher (see ADR-0019). One row per `Open Positions` option
-- entry on a broker statement upload.
--
-- Lives in core (alongside `prices`) rather than the prices plugin because
-- the dense `metrics_equity_position` MV reads it directly — the MV is
-- core, so its dependencies must apply during Phase A.
--
-- Payload shape (advisory; documented in
-- infra/risingwave/plugins/prices/payload_schema.json):
--   prices.option_mark: {"close": <double>, "currency": "USD"}
-- ============================================================================

-- v6: org_id propagates so per-tenant logical views can scope downstream
-- reads.
CREATE MATERIALIZED VIEW option_marks AS
    SELECT
        org_id,
        portfolio_id,
        source_id                                            AS instrument_id,
        observed_at                                          AS price_ts,
        ingest_ts,
        source,
        trace_id,
        (payload::jsonb ->> 'close')::DOUBLE PRECISION       AS close,
        payload::jsonb ->> 'currency'                        AS currency
    FROM data_log
    WHERE source_namespace = 'prices.option_mark';
