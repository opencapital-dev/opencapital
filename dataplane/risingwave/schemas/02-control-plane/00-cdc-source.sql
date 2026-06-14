-- ============================================================================
-- Postgres CDC source — pulls canonical reference rows from control_db.
--
-- v6 (Phase 2 of docs/v6/) repoints the source at control_db.public, the
-- new control-plane-canonical database. Publication: rw_v6_pub. Slot:
-- rw_v6_slot. The publication scope is exactly {portfolios};
-- instruments are now event-derived in RisingWave (ADR-0041).
--
-- The SOURCE ingests change events; per-table TABLE statements materialize
-- the rows. Keep this file numbered ahead of the *_cdc.sql tables that
-- depend on it.
-- ============================================================================

CREATE SOURCE IF NOT EXISTS pg_cdc WITH (
    connector = 'postgres-cdc',
    hostname  = '@@CDC_PG_HOST@@',
    port      = '5432',
    username  = 'rw_replicator',
    password  = 'rw_replicator',
    database.name    = 'control_db',
    schema.name      = 'public',
    publication.name = 'rw_v6_pub',
    slot.name        = 'rw_v6_slot'
);
