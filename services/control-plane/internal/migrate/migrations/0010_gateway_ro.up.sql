-- v6 Phase 4: gateway resolves portfolio_id -> org_id ownership via a
-- SELECT-only role against control_db.portfolios. The gateway holds no
-- other read or write privilege on this DB; compromise gives only the
-- portfolio mapping (ADR-0039 hardening note, accepted residual).

GRANT USAGE ON SCHEMA public TO gateway_ro;
GRANT SELECT ON portfolios TO gateway_ro;
