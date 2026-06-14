-- ============================================================================
-- portfolio_events_log — single core-owned event topic. v6 / Avro v2.
-- ----------------------------------------------------------------------------
-- v6 cutover (docs/v6/07-avro-v2-envelope.md, ADR-0037):
--   * topic: portfolio_events.v1 -> portfolio_events.v2
--   * envelope: gains required `org_id` (gateway-injected from the JWT,
--     ADR-0033/ADR-0038); drops `correction_of` (dead under v6 — every
--     correction is a re-publish under the same source_id via ADR-0006).
--   * SASL_PLAINTEXT auth on the Kafka listener (principal: rw_kafka).
--   * Schema Registry Basic Auth (principal: sr-rw).
--
-- v4 rename: v3 `portfolio_events` → `portfolio_events_log`. ADR-0028.
--
-- Encoding: Avro envelope + JSON payload string, FORMAT UPSERT. Symmetric
-- with data_log (adr/0011). The envelope is core-owned and strictly typed
-- (org_id, source_id, event_type, portfolio_id, instrument_id, business_ts,
-- ingest_ts, source, trace_id); the inner `payload` is a string containing
-- type-specific JSON. The kernel's marshalling.py validates payload shape
-- at fold time. The `events` MV casts payload to JSONB for downstream MV
-- consumption.
--
-- PK: the Kafka message key (`org_id|plugin_id|source_id`, UTF-8 bytes) —
-- exposed via INCLUDE KEY AS rw_key. The gateway stamps org_id and plugin_id
-- from the verified JWT into the key (datakey.EventKey), so a plugin can only
-- ever address its own org's rows — isolation is structural, not reliant on
-- source_id uniqueness.
--   * correction = re-publish under same key (ADR-0006).
--   * delete     = tombstone under same key.
-- ============================================================================

CREATE TABLE IF NOT EXISTS portfolio_events_log (
    org_id         VARCHAR,                -- gateway-stamped org UUID (v6)
    source_id      VARCHAR,
    event_type     VARCHAR,                -- TRADE | DIVIDEND | CASHFLOW | FX_CONVERSION | TRANSFER_IN | OPTION_*
    portfolio_id   VARCHAR,
    instrument_id  VARCHAR,                -- nullable for CASHFLOW / FX_CONVERSION
    business_ts    TIMESTAMPTZ,            -- unified ordering key
                                           --   = acquisition_date for TRANSFER_IN
                                           --   = event_ts otherwise
    ingest_ts      TIMESTAMPTZ,            -- refreshed on every upsert; gateway-stamped
    source         VARCHAR,                -- producer id (e.g. 'gateway@v6')
    plugin_id      VARCHAR,                -- gateway-stamped from the JWT plugin claim (Avro v2)
    trace_id       VARCHAR,
    payload        VARCHAR,                -- type-specific JSON; cast to JSONB in the events MV
    PRIMARY KEY (rw_key)
)
INCLUDE KEY AS rw_key
WITH (
    connector = 'kafka',
    topic = 'portfolio_events.v2',
    properties.bootstrap.server = 'redpanda:29092',
    -- SASL_PLAINTEXT + SCRAM-SHA-256. Password is templated in by
    -- apply.sh from infra/secrets/rw_kafka_sasl_password (SOPS
    -- materialised). RW `SECRET <name>` would require a paid
    -- license tier (4-core cap); inline literal substitution avoids
    -- that. SHOW CREATE leaks the password to anyone with RW root,
    -- which is already the same trust tier that reads RW data
    -- files directly.
    properties.security.protocol = 'SASL_PLAINTEXT',
    properties.sasl.mechanism = 'SCRAM-SHA-256',
    properties.sasl.username = 'rw_kafka',
    properties.sasl.password = '@@RW_KAFKA_SASL_PASSWORD@@',
    scan.startup.mode = 'earliest',
    properties.enable.auto.commit = 'false'
) FORMAT UPSERT ENCODE AVRO (
    -- Schema Registry HTTP Basic Auth. sr-rw is read-only on
    -- schemas. Password templated by apply.sh from
    -- infra/secrets/sr_rw_password.
    schema.registry = 'http://redpanda:8081',
    schema.registry.username = 'sr-rw',
    schema.registry.password = '@@SR_RW_PASSWORD@@'
);
