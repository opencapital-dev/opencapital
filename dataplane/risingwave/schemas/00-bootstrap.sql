-- ============================================================================
-- 00 — Bootstrap
-- ----------------------------------------------------------------------------
-- Migration tracker. Applied first by infra/risingwave/apply.sh.
--
-- v2 difference from v1: `plugin_id` column allows plugin-owned migrations
-- (infra/risingwave/plugins/<name>/migrations/) to be tracked independently
-- of core. See adr/0015-plugin-schema-fragments.md.
-- ============================================================================

CREATE TABLE IF NOT EXISTS _schema_migrations (
    plugin_id   VARCHAR NOT NULL DEFAULT 'core',  -- 'core' or plugin name
    version     VARCHAR NOT NULL,                  -- per-plugin version, e.g. 'V001'
    name        VARCHAR NOT NULL,                  -- migration description
    applied_at  TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (plugin_id, version)
);
