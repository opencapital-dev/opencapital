DROP PUBLICATION IF EXISTS rw_v6_pub;
REVOKE SELECT ON portfolios, instruments FROM rw_replicator;
REVOKE USAGE ON SCHEMA public FROM rw_replicator;
