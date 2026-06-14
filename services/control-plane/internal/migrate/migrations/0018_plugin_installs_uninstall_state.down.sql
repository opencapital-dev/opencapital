ALTER TABLE plugin_installs
    DROP COLUMN uninstall_state,
    DROP COLUMN uninstall_started_at,
    DROP COLUMN uninstall_offset_events,
    DROP COLUMN uninstall_offset_data,
    DROP COLUMN uninstall_keys_total,
    DROP COLUMN uninstall_keys_done,
    DROP COLUMN uninstall_last_error;
