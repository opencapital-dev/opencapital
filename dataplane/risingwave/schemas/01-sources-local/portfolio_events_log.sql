-- ============================================================================
-- portfolio_events_log — LOCAL packaging (connector-less). Identical table to
-- schemas/01-sources/portfolio_events_log.sql minus the Kafka connector.
-- ----------------------------------------------------------------------------
-- The fully-local desktop data plane has no Redpanda. The gateway writes events
-- straight into this table over pgwire (SINK_MODE=rw): INSERT with an existing
-- rw_key overwrites (RW's native upsert-on-PK), and DELETE by rw_key retracts
-- through every downstream MV — reproducing the cloud FORMAT UPSERT + null-value
-- tombstone contract with no normalization layer.
--
-- Same name, same typed columns, same PRIMARY KEY (rw_key) as the cloud table,
-- so every MV from 02-* onward is byte-for-byte identical across packagings.
-- The only difference vs cloud: rw_key is a normal column the gateway sets
-- (datakey.EventKey), not an INCLUDE KEY projection of the Kafka message key.
-- See docs/superpowers/specs/2026-06-14-fully-local-desktop-data-plane-design.md
-- ============================================================================

CREATE TABLE IF NOT EXISTS portfolio_events_log (
    org_id         VARCHAR,                -- gateway-stamped org UUID
    source_id      VARCHAR,
    event_type     VARCHAR,                -- TRADE | DIVIDEND | CASHFLOW | FX_CONVERSION | TRANSFER_IN | OPTION_*
    portfolio_id   VARCHAR,
    instrument_id  VARCHAR,                -- nullable for CASHFLOW / FX_CONVERSION
    business_ts    TIMESTAMPTZ,            -- unified ordering key
    ingest_ts      TIMESTAMPTZ,            -- refreshed on every upsert; gateway-stamped
    source         VARCHAR,                -- producer id (e.g. 'gateway@v6')
    plugin_id      VARCHAR,                -- gateway-stamped from the JWT plugin claim
    trace_id       VARCHAR,
    payload        VARCHAR,                -- type-specific JSON; cast to JSONB in the events MV
    rw_key         VARCHAR,                -- org_id|plugin_id|source_id (gateway sets it)
    PRIMARY KEY (rw_key)
);
