-- User-added federated plugin sources. Each row is a per-plugin manifest URL the
-- user subscribed to. The OFFICIAL set is NOT stored here — it is the live
-- plugins.json list fetch. Registry coords + versions live in each manifest
-- (spec §3); only the URL + cached display publisher are stored.
CREATE TABLE IF NOT EXISTS plugin_sources (
    manifest_url TEXT PRIMARY KEY,
    publisher    TEXT NOT NULL DEFAULT '',
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    added_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
