-- ============================================================================
-- portfolios — CDC-derived from control_db.public.portfolios.
-- Single-user/local: no org_id. PK is portfolio_id alone.
-- ============================================================================

CREATE TABLE IF NOT EXISTS portfolios (
    portfolio_id    VARCHAR,
    base_currency   VARCHAR,
    attributes      JSONB,
    updated_at      TIMESTAMPTZ,
    updated_by      VARCHAR,
    PRIMARY KEY (portfolio_id)
)
FROM pg_cdc TABLE 'public.portfolios';
