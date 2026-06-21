CREATE TABLE IF NOT EXISTS portfolios (
    portfolio_id  UUID PRIMARY KEY,
    base_currency VARCHAR NOT NULL,
    attributes    JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by    VARCHAR NOT NULL DEFAULT 'core'
);
GRANT USAGE ON SCHEMA public TO rw_replicator;
GRANT SELECT ON portfolios TO rw_replicator;
DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname='rw_v6_pub') THEN CREATE PUBLICATION rw_v6_pub FOR TABLE portfolios; END IF; END $$;
