-- Phase 2: expose canonical tables for RisingWave's pg_cdc connector.
-- ADR-0041 documents the rw_replicator scope as bounded tech debt.

GRANT USAGE ON SCHEMA public TO rw_replicator;
GRANT SELECT ON portfolios, instruments TO rw_replicator;

CREATE PUBLICATION rw_v6_pub FOR TABLE portfolios, instruments;
