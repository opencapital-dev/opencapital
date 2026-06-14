ALTER TABLE plugin_installs RENAME COLUMN granted_at TO installed_at;
ALTER TABLE plugin_installs ADD COLUMN version TEXT NOT NULL DEFAULT 'unknown';
