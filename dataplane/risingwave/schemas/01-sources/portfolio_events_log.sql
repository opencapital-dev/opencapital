-- ============================================================================
-- portfolio_events_log — connector-less landing table (local desktop data plane).
-- ----------------------------------------------------------------------------
-- Plugins INSERT directly over pgwire (no Kafka, no gateway). An INSERT with
-- an existing rw_key overwrites (RW native upsert-on-PK); a DELETE by rw_key
-- retracts through every downstream MV — reproducing the FORMAT UPSERT
-- tombstone contract with no broker layer.
--
-- Column contract (no org_id — portfolio_id is the sole scope key):
--   portfolio_events_log(source_id, event_type, portfolio_id, instrument_id,
--     business_ts, ingest_ts, source, plugin_id, trace_id, payload, rw_key PK)
-- ============================================================================

CREATE TABLE IF NOT EXISTS portfolio_events_log (
    source_id      VARCHAR,
    event_type     VARCHAR,                -- TRADE | DIVIDEND | CASHFLOW | FX_CONVERSION | TRANSFER_IN | OPTION_*
    portfolio_id   VARCHAR,
    instrument_id  VARCHAR,                -- nullable for CASHFLOW / FX_CONVERSION
    business_ts    TIMESTAMPTZ,            -- unified ordering key
    ingest_ts      TIMESTAMPTZ,            -- refreshed on every upsert; plugin-stamped
    source         VARCHAR,                -- producer id (e.g. 'core@v1')
    plugin_id      VARCHAR,                -- plugin-stamped from the plugin claim
    trace_id       VARCHAR,
    payload        VARCHAR,                -- type-specific JSON; cast to JSONB in the events MV
    rw_key         VARCHAR,                -- plugin_id|source_id (plugin sets it)
    PRIMARY KEY (rw_key)
);
