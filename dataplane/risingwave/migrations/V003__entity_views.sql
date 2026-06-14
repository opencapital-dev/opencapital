-- Entity-view layer: clean domain nouns over per-tick MVs.
-- Plain projections (no recompute) with uniform column conventions:
--   org_id       tenant scope
--   portfolio_id scope column (aliased from scope_id where the MV uses it)
--   ts           time axis  (aliased from event_ts / price_ts / business_ts)
-- All views are idempotent (CREATE VIEW IF NOT EXISTS).

-- ---- portfolio -------------------------------------------------------
CREATE VIEW IF NOT EXISTS e_portfolio AS
SELECT
    org_id,
    scope_id                  AS portfolio_id,
    event_ts                  AS ts,
    base_currency,
    nav_base,
    equity_value_base,
    cash_value_base,
    instrument_count,
    total_gross_avg_base,
    total_net_avg_base,
    realized_equity_avg_base,
    realized_forex_avg_base,
    unrealized_equity_avg_base,
    unrealized_forex_avg_base,
    realized_interest_base,
    realized_dividends_base,
    fees_base
FROM portfolio_per_tick;

-- ---- instrument ------------------------------------------------------
-- NOTE: irr_annualized is absent from instrument_per_tick; excluded here.
CREATE VIEW IF NOT EXISTS e_instrument AS
SELECT
    org_id,
    scope_id                    AS portfolio_id,
    instrument_id,
    event_ts                    AS ts,
    direction,
    quantity,
    currency,
    avg_cost_avg_native,
    avg_cost_avg_base,
    last_price,
    realized_equity_avg_native,
    realized_equity_avg_base,
    realized_forex_avg_base,
    unrealized_equity_avg_native,
    unrealized_equity_avg_base,
    unrealized_forex_avg_base,
    lot_count
FROM instrument_per_tick;

-- ---- cash ------------------------------------------------------------
CREATE VIEW IF NOT EXISTS e_cash AS
SELECT
    org_id,
    scope_id                      AS portfolio_id,
    event_ts                      AS ts,
    currency,
    base_currency,
    balance_native,
    cash_value_base,
    realized_interest_base,
    realized_dividends_base,
    realized_fx_avg_base,
    unrealized_fx_avg_base,
    fees_base,
    deposits_cumulative_native,
    withdrawals_cumulative_native
FROM cash_per_tick;

-- ---- price -----------------------------------------------------------
CREATE VIEW IF NOT EXISTS e_price AS
SELECT
    org_id,
    portfolio_id,
    instrument_id,
    price_ts   AS ts,
    price      AS value,
    currency
FROM prices;

-- ---- events ----------------------------------------------------------
-- payload is already jsonb in the events MV.
CREATE VIEW IF NOT EXISTS e_events AS
SELECT
    org_id,
    portfolio_id,
    instrument_id,
    business_ts   AS ts,
    event_type,
    payload,
    base_currency
FROM events;

-- ---- nav (narrow; List<Point> binding) -------------------------------
CREATE VIEW IF NOT EXISTS e_nav AS
SELECT
    org_id,
    scope_id   AS portfolio_id,
    event_ts   AS ts,
    nav_base   AS value
FROM portfolio_per_tick
WHERE nav_base > 0;

-- ---- flows (narrow; cashflow/transfer-in classification) -------------
CREATE VIEW IF NOT EXISTS e_flows AS
SELECT
    org_id,
    portfolio_id,
    business_ts   AS ts,
    COALESCE((payload ->> 'amount')::DOUBLE PRECISION, 0.0) AS amt
FROM events
WHERE (event_type = 'CASHFLOW' AND (payload ->> 'type') IN ('DEPOSIT', 'WITHDRAWAL'))
   OR event_type = 'TRANSFER_IN';
