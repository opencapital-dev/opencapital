-- ============================================================================
-- v6 Phase 6 -- physical streaming replication.
--
-- Adds a dedicated REPLICATION-capable role for the postgres-replica
-- standby. Kept separate from `rw_replicator` (which holds the logical
-- publication SELECT grants for RisingWave CDC) so a compromise of one
-- doesn't grant the other's surface. The pg_hba entry that permits the
-- standby to connect lives in 04-replication-hba.sh next door.
-- ============================================================================

CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD 'replicator_pw';
