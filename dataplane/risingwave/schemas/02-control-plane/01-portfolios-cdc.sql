-- ============================================================================
-- portfolios — CDC-derived from control_db.public.portfolios.
-- v6 PK is (org_id, portfolio_id); the leading org_id column carries the
-- multi-tenant scope into the streaming graph. RW's pg_cdc connector
-- serialises both as text on the wire; the column type stays VARCHAR.
-- ============================================================================

CREATE TABLE IF NOT EXISTS portfolios (
    org_id          VARCHAR,
    portfolio_id    VARCHAR,
    base_currency   VARCHAR,
    attributes      JSONB,
    updated_at      TIMESTAMPTZ,
    updated_by      VARCHAR,
    PRIMARY KEY (org_id, portfolio_id)
)
FROM pg_cdc TABLE 'public.portfolios';
