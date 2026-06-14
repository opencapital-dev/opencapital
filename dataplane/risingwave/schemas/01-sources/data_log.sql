-- ============================================================================
-- data_log — single plugin-extensible external-observation topic. v6 / Avro v2.
-- ----------------------------------------------------------------------------
-- v6 cutover (docs/v6/07-avro-v2-envelope.md, ADR-0037):
--   * topic: data.v1 -> data.v2
--   * envelope: gains required `org_id` (gateway-injected from the JWT,
--     ADR-0033/ADR-0038).
--   * SASL_PLAINTEXT auth on the Kafka listener (principal: rw_kafka).
--   * Schema Registry Basic Auth (principal: sr-rw).
--
-- Kafka-fed (data.v2 topic). Generalised shape so plugins can publish
-- anything: prices today, Polymarket markets, weather, satellite-derived
-- signals, alt-data — anything that can be expressed as "this entity, at
-- this moment, had these properties".
--
-- v4 rename: v3 `data` → `data_log`. ADR-0028 (`_log` suffix policy):
-- reserved for Kafka-fed log streams. Still upsertable via Kafka key
-- (FORMAT UPSERT) — corrections propagate by re-publishing under the same
-- key (ADR-0006).
--
-- Encoding: Avro envelope + JSON payload. The envelope is core-owned and
-- never changes; the payload is a string holding plugin-specific JSON.
-- Plugins document their JSON shape in payload_schema.json (advisory only).
-- See adr/0011-avro-envelope-json-payload-for-data.md for rationale.
--
-- Critical property (option 2 + adr/0012): nothing in the fold reads
-- `data_log`. The fold has no transitive dependency on this topic.
-- `data_log` only flows into MtM-layer MVs and plugin-owned MVs.
-- ============================================================================

CREATE TABLE IF NOT EXISTS data_log (
    org_id            VARCHAR,             -- gateway-stamped org UUID (v6)
    source_namespace  VARCHAR,             -- 'prices.quote', 'prices.ohlcv', 'polymarket', …
    source_id         VARCHAR,             -- entity id within the namespace
    portfolio_id      VARCHAR,             -- nullable; set when data is portfolio-scoped (v8 Track 2c)
    observed_at       TIMESTAMPTZ,         -- when the world was in this state
    ingest_ts         TIMESTAMPTZ,         -- when we recorded it; gateway-stamped
    source            VARCHAR,             -- producer id (e.g. 'gateway@v6')
    plugin_id         VARCHAR,             -- gateway-stamped from the JWT plugin claim (Avro v2)
    trace_id          VARCHAR,
    payload           VARCHAR,             -- raw JSON; cast to JSONB in consuming MVs
    PRIMARY KEY (rw_key)
)
INCLUDE KEY AS rw_key
WITH (
    connector = 'kafka',
    topic = 'data.v2',
    properties.bootstrap.server = 'redpanda:29092',
    -- SASL_PLAINTEXT + SCRAM-SHA-256, same shape as
    -- portfolio_events_log. Passwords templated by apply.sh from
    -- infra/secrets/{rw_kafka_sasl_password,sr_rw_password}.
    properties.security.protocol = 'SASL_PLAINTEXT',
    properties.sasl.mechanism = 'SCRAM-SHA-256',
    properties.sasl.username = 'rw_kafka',
    properties.sasl.password = '@@RW_KAFKA_SASL_PASSWORD@@',
    scan.startup.mode = 'earliest',
    -- librdkafka auto-commit disabled so RW's barrier protocol is the sole
    -- offset authority. See [[project_rw_kafka_auto_commit]]. Topic seeding
    -- (a REAL Avro message, not a null tombstone) is done in topics-init —
    -- RW issue #18299: when the source is created against an empty topic
    -- or one with only-tombstones, the split state persists `start_offset=-1`
    -- and the executor parks; subsequent publishes never get consumed.
    -- A real-row warmup forces RW to advance start_offset past -1.
    properties.enable.auto.commit = 'false'
) FORMAT UPSERT ENCODE AVRO (
    schema.registry = 'http://redpanda:8081',
    schema.registry.username = 'sr-rw',
    schema.registry.password = '@@SR_RW_PASSWORD@@'
);
