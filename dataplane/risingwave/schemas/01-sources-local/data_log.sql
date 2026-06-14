-- ============================================================================
-- data_log — LOCAL packaging (connector-less). Identical table to
-- schemas/01-sources/data_log.sql minus the Kafka connector.
-- ----------------------------------------------------------------------------
-- The fully-local desktop data plane has no Redpanda. The gateway writes data
-- observations straight into this table over pgwire (SINK_MODE=rw): INSERT
-- upserts on rw_key, DELETE by rw_key tombstones — same contract as the cloud
-- FORMAT UPSERT Kafka source, so every consuming MV is unchanged.
--
-- rw_key is a normal column the gateway sets (datakey.DataKey), not an
-- INCLUDE KEY projection of the Kafka message key.
-- ============================================================================

CREATE TABLE IF NOT EXISTS data_log (
    org_id            VARCHAR,             -- gateway-stamped org UUID
    source_namespace  VARCHAR,             -- 'prices.quote', 'prices.ohlcv', 'polymarket', …
    source_id         VARCHAR,             -- entity id within the namespace
    portfolio_id      VARCHAR,             -- nullable; set when data is portfolio-scoped
    observed_at       TIMESTAMPTZ,         -- when the world was in this state
    ingest_ts         TIMESTAMPTZ,         -- when we recorded it; gateway-stamped
    source            VARCHAR,             -- producer id (e.g. 'gateway@v6')
    plugin_id         VARCHAR,             -- gateway-stamped from the JWT plugin claim
    trace_id          VARCHAR,
    payload           VARCHAR,             -- raw JSON; cast to JSONB in consuming MVs
    rw_key            VARCHAR,             -- org_id|plugin_id|namespace|portfolio|source_id|observed_at
    PRIMARY KEY (rw_key)
);
