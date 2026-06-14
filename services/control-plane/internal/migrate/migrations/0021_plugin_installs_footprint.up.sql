-- v8: snapshot the plugin's install footprint (its self-describing
-- control-plane.json) on the install row, so Uninstall can drop the
-- per-org RW views without re-reading the OCI registry (the plugin may
-- have been un-published by then). Empty default for pre-existing rows;
-- they get backfilled on their next reinstall.
ALTER TABLE plugin_installs
    ADD COLUMN IF NOT EXISTS footprint JSONB NOT NULL DEFAULT '{}'::jsonb;
