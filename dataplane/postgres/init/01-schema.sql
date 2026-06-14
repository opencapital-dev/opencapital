-- ============================================================================
-- v6 Postgres bootstrap (Phase 2 of docs/v6/).
--
-- Runs once at postgres-container init time as the `postgres` superuser
-- against the database named by POSTGRES_DB (`control_db`). All tables
-- and the CDC publication move into control-plane migrations under
-- services/control-plane/internal/migrate/migrations/.
--
-- Two roles:
--   * `control_plane` -- owns control_db, runs every migration, holds
--     INSERT/UPDATE/DELETE on portfolios + instruments.
--   * `rw_replicator` -- REPLICATION-capable login that RisingWave's
--     pg_cdc connector authenticates as. SELECT grants on the canonical
--     tables are applied by control-plane migration 0007 once the tables
--     exist (control_plane owns the tables, so the grant is within its
--     rights without superuser escalation).
-- ============================================================================

CREATE ROLE control_plane WITH LOGIN PASSWORD 'control_plane_pw';
CREATE ROLE rw_replicator WITH REPLICATION LOGIN PASSWORD 'rw_replicator';
-- gateway_ro: SELECT-only on control_db.portfolios. Read by the v6 gateway
-- to resolve portfolio_id -> org_id ownership (ADR-0039). The grant on
-- the portfolios table is applied by control-plane migration 0010 once
-- the table exists; this role declaration only sets up the login.
CREATE ROLE gateway_ro WITH LOGIN PASSWORD 'gateway_ro';

ALTER DATABASE control_db OWNER TO control_plane;
GRANT CONNECT ON DATABASE control_db TO rw_replicator;
GRANT CONNECT ON DATABASE control_db TO gateway_ro;
