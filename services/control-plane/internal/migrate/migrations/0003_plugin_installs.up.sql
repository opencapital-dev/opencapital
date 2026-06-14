CREATE TABLE IF NOT EXISTS plugin_installs (
    org_id       UUID NOT NULL REFERENCES organisations(org_id) ON DELETE CASCADE,
    plugin_id    TEXT NOT NULL,
    version      TEXT NOT NULL DEFAULT 'unknown',
    installed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org_id, plugin_id)
);
