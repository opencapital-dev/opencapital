-- ============================================================================
-- data_log — connector-less landing table (local desktop data plane).
-- ----------------------------------------------------------------------------
-- Plugins INSERT directly over pgwire (no Kafka, no gateway). INSERT upserts
-- on rw_key; DELETE by rw_key tombstones — same contract as FORMAT UPSERT,
-- so every consuming MV is unchanged.
--
-- Column contract (no org_id — portfolio_id is the sole scope key):
--   data_log(source_namespace, source_id, portfolio_id, observed_at, ingest_ts,
--     source, plugin_id, trace_id, payload, rw_key PK)
-- ============================================================================

CREATE TABLE IF NOT EXISTS data_log (
    source_namespace  VARCHAR,             -- 'prices.quote', 'prices.ohlcv', 'polymarket', …
    source_id         VARCHAR,             -- entity id within the namespace
    portfolio_id      VARCHAR,             -- nullable; set when data is portfolio-scoped
    observed_at       TIMESTAMPTZ,         -- when the world was in this state
    ingest_ts         TIMESTAMPTZ,         -- when we recorded it; plugin-stamped
    source            VARCHAR,             -- producer id (e.g. 'core@v1')
    plugin_id         VARCHAR,             -- plugin-stamped from the plugin claim
    trace_id          VARCHAR,
    payload           VARCHAR,             -- raw JSON; cast to JSONB in consuming MVs
    rw_key            VARCHAR,             -- plugin_id|namespace|portfolio|source_id|observed_at
    PRIMARY KEY (rw_key)
);
