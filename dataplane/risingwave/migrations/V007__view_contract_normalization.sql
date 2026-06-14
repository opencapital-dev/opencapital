-- Normalize all queryable entity views to the view-as-contract shape:
--   org_id        mandatory tenancy column
--   portfolio     canonical scope alias (from scope_id / portfolio_id)
--   instrument    canonical instrument alias (from instrument_id where applicable)
--   ts            bigint microseconds since epoch (replaces timestamp columns)
--   value         single-value series alias
-- portfolio_id kept alongside portfolio where a live consumer reads it as data
-- from result rows (discovery.go, handlers_ref.go, backfill_worker.go).
-- Views are plain projections; DROP + CREATE is safe (gateway is sole reader).

-- ---- e_portfolio -----------------------------------------------------------
DROP VIEW IF EXISTS e_portfolio;
CREATE VIEW e_portfolio AS
SELECT
    org_id,
    scope_id                                          AS portfolio,
    (extract(epoch from event_ts) * 1000000)::bigint  AS ts,
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

-- ---- e_nav -----------------------------------------------------------------
DROP VIEW IF EXISTS e_nav;
CREATE VIEW e_nav AS
SELECT
    org_id,
    scope_id                                          AS portfolio,
    (extract(epoch from event_ts) * 1000000)::bigint  AS ts,
    nav_base                                          AS value
FROM portfolio_per_tick
WHERE nav_base > 0;

-- ---- e_instrument ----------------------------------------------------------
DROP VIEW IF EXISTS e_instrument;
CREATE VIEW e_instrument AS
SELECT
    org_id,
    scope_id                                          AS portfolio,
    instrument_id                                     AS instrument,
    (extract(epoch from event_ts) * 1000000)::bigint  AS ts,
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
    lot_count,
    quantity * avg_cost_avg_base                      AS position_size_base,
    equity_value_base                                 AS current_value,
    unrealized_equity_avg_base                        AS unrealized_pnl
FROM instrument_per_tick;

-- ---- e_cash ----------------------------------------------------------------
DROP VIEW IF EXISTS e_cash;
CREATE VIEW e_cash AS
SELECT
    org_id,
    scope_id                                          AS portfolio,
    (extract(epoch from event_ts) * 1000000)::bigint  AS ts,
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

-- ---- e_price ---------------------------------------------------------------
DROP VIEW IF EXISTS e_price;
CREATE VIEW e_price AS
SELECT
    org_id,
    portfolio_id                                          AS portfolio,
    instrument_id                                         AS instrument,
    (extract(epoch from price_ts) * 1000000)::bigint      AS ts,
    price                                                 AS value,
    currency
FROM prices;

-- ---- e_events --------------------------------------------------------------
DROP VIEW IF EXISTS e_events;
CREATE VIEW e_events AS
SELECT
    org_id,
    portfolio_id                                          AS portfolio,
    instrument_id                                         AS instrument,
    (extract(epoch from business_ts) * 1000000)::bigint   AS ts,
    event_type,
    payload,
    base_currency
FROM events;

-- ---- e_flows ---------------------------------------------------------------
DROP VIEW IF EXISTS e_flows;
CREATE VIEW e_flows AS
SELECT
    org_id,
    portfolio_id                                            AS portfolio,
    (extract(epoch from business_ts) * 1000000)::bigint     AS ts,
    COALESCE(payload ->> 'type', event_type)                AS flow_type,
    CASE WHEN (payload ->> 'type') = 'WITHDRAWAL'
         THEN -COALESCE((payload ->> 'amount_native')::DOUBLE PRECISION, 0.0)
         ELSE  COALESCE((payload ->> 'amount_native')::DOUBLE PRECISION, 0.0)
    END                                                     AS amt
FROM events
WHERE (event_type = 'CASHFLOW' AND (payload ->> 'type') IN ('DEPOSIT', 'WITHDRAWAL'))
   OR event_type = 'TRANSFER_IN';

-- ---- e_closures ------------------------------------------------------------
DROP VIEW IF EXISTS e_closures;
CREATE VIEW e_closures AS
SELECT
    org_id,
    portfolio_id                                           AS portfolio,
    instrument_id                                          AS instrument,
    (extract(epoch from exit_ts) * 1000000)::bigint        AS ts,
    realized_pnl_base                                      AS realized_pnl,
    extract(epoch from (exit_ts - entry_ts))               AS holding_seconds
FROM closures_per_event;

-- ---- e_cycles --------------------------------------------------------------
DROP VIEW IF EXISTS e_cycles;
CREATE VIEW e_cycles AS
SELECT
    org_id,
    portfolio_id                                           AS portfolio,
    instrument_id                                          AS instrument,
    (extract(epoch from close_ts) * 1000000)::bigint       AS ts,
    pnl_base,
    duration_sec,
    CASE WHEN was_re_entry THEN 1.0 ELSE 0.0 END           AS was_re_entry
FROM cycles_per_event;

-- ---- instruments_catalog ---------------------------------------------------
DROP VIEW IF EXISTS instruments_catalog;
CREATE VIEW instruments_catalog AS
SELECT u.org_id,
       u.portfolio_id                                          AS portfolio,
       u.portfolio_id                                          AS portfolio_id,
       u.instrument_id                                         AS instrument,
       u.instrument_id                                         AS instrument_id,
       i.kind,
       i.contract_multiplier,
       ccy.currency,
       ccy.base_currency,
       (extract(epoch from u.first_seen_ts) * 1000000)::bigint AS first_seen_ts,
       (extract(epoch from u.last_seen_ts)  * 1000000)::bigint AS last_seen_ts,
       (extract(epoch from u.last_seen_ts)  * 1000000)::bigint AS ts,
       u.event_count
FROM instruments_used u
LEFT JOIN instruments i
  ON i.org_id = u.org_id
 AND i.portfolio_id = u.portfolio_id
 AND i.instrument_id = u.instrument_id
LEFT JOIN (
    SELECT DISTINCT org_id, scope_id AS portfolio_id, instrument_id, currency, base_currency
    FROM instrument_per_event
) ccy
  ON ccy.org_id = u.org_id
 AND ccy.portfolio_id = u.portfolio_id
 AND ccy.instrument_id = u.instrument_id;

-- ---- ohlcv_coverage --------------------------------------------------------
DROP VIEW IF EXISTS ohlcv_coverage;
CREATE VIEW ohlcv_coverage AS
SELECT org_id,
       plugin_id,
       portfolio_id                                          AS portfolio,
       portfolio_id                                          AS portfolio_id,
       source_id,
       source_id                                             AS instrument,
       (extract(epoch from observed_at) * 1000000)::bigint   AS observed_at,
       (extract(epoch from observed_at) * 1000000)::bigint   AS ts
FROM data_log
WHERE source_namespace = 'prices.ohlcv';

-- ---- data_coverage ---------------------------------------------------------
DROP VIEW IF EXISTS data_coverage;
CREATE VIEW data_coverage AS
SELECT org_id,
       plugin_id,
       portfolio_id                                          AS portfolio,
       source_id,
       source_id                                             AS instrument,
       source_namespace,
       (extract(epoch from observed_at) * 1000000)::bigint   AS observed_at,
       (extract(epoch from observed_at) * 1000000)::bigint   AS ts
FROM data_log
WHERE observed_at >= '2000-01-01' AND observed_at < '2100-01-01';

-- ---- fx_pairs_used (MV) ----------------------------------------------------
-- fx_pairs_used is a MV (no portfolio scope). Timestamps converted to bigint
-- µs directly in the aggregation so the wire carries integers, not timestamps.
DROP MATERIALIZED VIEW IF EXISTS fx_pairs_used;
CREATE MATERIALIZED VIEW fx_pairs_used AS
    SELECT
        e.org_id                                                        AS org_id,
        p.base_currency                                                 AS base_ccy,
        UPPER(e.payload ->> 'currency')                                 AS quote_ccy,
        (extract(epoch from MIN(e.business_ts)) * 1000000)::bigint      AS first_seen_ts,
        (extract(epoch from MAX(e.business_ts)) * 1000000)::bigint      AS last_seen_ts,
        (extract(epoch from MAX(e.business_ts)) * 1000000)::bigint      AS ts,
        p.base_currency                                                 AS base,
        UPPER(e.payload ->> 'currency')                                 AS quote,
        COUNT(*)                                                        AS event_count
    FROM events e
    JOIN portfolios p USING (org_id, portfolio_id)
    WHERE e.event_type IN ('TRADE', 'DIVIDEND')
      AND e.payload ->> 'currency' IS NOT NULL
      AND UPPER(e.payload ->> 'currency') <> p.base_currency
    GROUP BY e.org_id, p.base_currency, UPPER(e.payload ->> 'currency');
