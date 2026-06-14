-- instruments: event-derived, portfolio-scoped instrument identity. Replaces
-- the former CDC mirror of control_db.instruments. Resolves the only two
-- attributes the calc pipeline consumes -- kind + contract_multiplier -- from
-- the event payload. No control-plane dependency.
--
-- Keyed (org_id, portfolio_id, instrument_id): instrument_id is a broker-local
-- ticker (T212 ticker / IBKR OCC symbol), not a canonical security key, so it
-- collides across portfolios fed by different brokers; per-portfolio scope
-- isolates each. kind/contract_multiplier are immutable per instrument, so an
-- aggregate over the portfolio's events yields the (consistent) value; events
-- without the fields default to equity/1.
CREATE MATERIALIZED VIEW IF NOT EXISTS instruments AS
SELECT
    org_id,
    portfolio_id,
    instrument_id,
    COALESCE(max((payload::jsonb) ->> 'instrument_kind'), 'equity')           AS kind,
    COALESCE(max(((payload::jsonb) ->> 'contract_multiplier')::DECIMAL), 1.0) AS contract_multiplier
FROM portfolio_events_log
WHERE instrument_id IS NOT NULL
GROUP BY org_id, portfolio_id, instrument_id;
