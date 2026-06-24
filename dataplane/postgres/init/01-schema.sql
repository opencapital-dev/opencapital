-- ============================================================================
-- v6 Postgres bootstrap (Phase 2 of docs/v6/).
--
-- Runs at data-plane boot as the `postgres` superuser against `control_db`.
-- The portfolios table + the rw_v6_pub CDC publication are created by the
-- host reconciler via 02-portfolios.sql (control-plane was removed).
--
-- Two roles:
--   * `control_plane` -- owns control_db, runs every migration, holds
--     INSERT/UPDATE/DELETE on its tables (e.g. portfolios).
--   * `rw_replicator` -- REPLICATION-capable login that RisingWave's
--     pg_cdc connector authenticates as. SELECT grants on the canonical
--     tables are applied by control-plane migration 0007 once the tables
--     exist (control_plane owns the tables, so the grant is within its
--     rights without superuser escalation).
-- ============================================================================

CREATE ROLE control_plane WITH LOGIN PASSWORD 'control_plane_pw';
CREATE ROLE rw_replicator WITH REPLICATION LOGIN PASSWORD 'rw_replicator';

ALTER DATABASE control_db OWNER TO control_plane;
GRANT CONNECT ON DATABASE control_db TO rw_replicator;
