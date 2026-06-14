-- v6 Phase 8 self-service plugin uninstall is async + resumable. The
-- worker pages through every Kafka key the plugin produced (filtered
-- on plugin_id + org_id via RW) and emits a tombstone per key via the
-- gateway. Pages are committed to RW eventual-consistently. To survive
-- a control-plane restart mid-uninstall, the per-(org, plugin) row
-- carries the checkpoint inline.
--
-- uninstall_state:
--   NULL  -> not uninstalling
--   'in_progress'
--   'failed' (terminal; operator inspects, optionally retries)
-- The 'done' state is materialised as DELETE FROM plugin_installs
-- after the worker finishes, so no 'done' literal needed.

ALTER TABLE plugin_installs
    ADD COLUMN uninstall_state        TEXT,
    ADD COLUMN uninstall_started_at   TIMESTAMPTZ,
    ADD COLUMN uninstall_offset_events INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN uninstall_offset_data   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN uninstall_keys_total    INTEGER,
    ADD COLUMN uninstall_keys_done     INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN uninstall_last_error    TEXT;
