-- ============================================================================
-- fx_rates — currency-pair broker FX rate index.
-- ----------------------------------------------------------------------------
-- v4: reads `portfolio_events_log` directly (not `events`). v3 had this MV
-- read the projection-only `events` MV; v4 eliminated that projection layer
-- and renamed the enriched MV to `events`, which would have created a cycle
-- (`events` ASOF-joins fx_rates; if fx_rates read `events`, the streaming
-- graph would be cyclic). Reading the raw Kafka-fed table directly breaks
-- the cycle without reintroducing an intermediate projection MV. The
-- `payload::jsonb` cast happens inline per row in each UNION branch.
--
-- ADR-0012 (producer-side FX) unchanged: this MV is the BROKER-rate index
-- used by:
--   - `events` (event-time ASOF fallback when payload lacks `fx_rate_to_base`)
--   - `cash_per_tick`, `instrument_per_tick`, `portfolio_per_tick` (tick-time
--     ASOF marks for mark-to-market cash and equity values)
--   - `cash_per_tick` tick-grid derivation (`SELECT FROM fx_rates`)
--
-- ADR-0005 (event-vs-MtM seam) intact: reads only `portfolio_events_log` +
-- `portfolios`. No path (direct or transitive) to `data_log`. The fold's
-- structural seam holds.
--
-- v1 had 6 branches (market direct/inverse from prices + 4 broker). v2/v3/v4
-- keep only the 4 broker branches.
--
-- v6: every branch carries org_id so the downstream `events` ASOF join is
-- naturally org-scoped (an event of org A cannot ASOF-match an FX row of
-- org B). The ASOF in events.sql does not include org_id in the join
-- predicate today; it is safe because the same (portfolio_id ->
-- base_currency) mapping is org-scoped via the portfolios CDC table,
-- but propagating org_id through fx_rates keeps the option open to add
-- the predicate later without a re-shape.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS fx_rates AS
      -- broker rate from trades / dividends: currency -> portfolio base_currency
      SELECT e.org_id                                                     AS org_id,
             ((e.payload::jsonb) ->> 'currency')                          AS from_ccy,
             p.base_currency                                              AS to_ccy,
             e.business_ts                                                AS ts,
             ((e.payload::jsonb) ->> 'fx_rate_to_base')::DOUBLE PRECISION AS rate,
             'broker'                                                     AS source
      FROM portfolio_events_log e
      JOIN portfolios p USING (org_id, portfolio_id)
      WHERE e.event_type IN ('TRADE', 'DIVIDEND')
        AND (e.payload::jsonb) ->> 'fx_rate_to_base' IS NOT NULL
    UNION ALL
      -- broker rate inverse from trades / dividends
      SELECT e.org_id                                                     AS org_id,
             p.base_currency                                              AS from_ccy,
             ((e.payload::jsonb) ->> 'currency')                          AS to_ccy,
             e.business_ts                                                AS ts,
             1.0 / ((e.payload::jsonb) ->> 'fx_rate_to_base')::DOUBLE PRECISION AS rate,
             'broker_inverse'                                             AS source
      FROM portfolio_events_log e
      JOIN portfolios p USING (org_id, portfolio_id)
      WHERE e.event_type IN ('TRADE', 'DIVIDEND')
        AND (e.payload::jsonb) ->> 'fx_rate_to_base' IS NOT NULL
        AND ((e.payload::jsonb) ->> 'fx_rate_to_base')::DOUBLE PRECISION > 0
    UNION ALL
      -- broker rate from fx-conversions: implied from_currency -> to_currency
      SELECT org_id                                                       AS org_id,
             ((payload::jsonb) ->> 'from_currency')                       AS from_ccy,
             ((payload::jsonb) ->> 'to_currency')                         AS to_ccy,
             business_ts                                                  AS ts,
             ((payload::jsonb) ->> 'rate')::DOUBLE PRECISION              AS rate,
             'broker'                                                     AS source
      FROM portfolio_events_log
      WHERE event_type = 'FX_CONVERSION'
    UNION ALL
      -- broker rate inverse from fx-conversions
      SELECT org_id                                                       AS org_id,
             ((payload::jsonb) ->> 'to_currency')                         AS from_ccy,
             ((payload::jsonb) ->> 'from_currency')                       AS to_ccy,
             business_ts                                                  AS ts,
             1.0 / ((payload::jsonb) ->> 'rate')::DOUBLE PRECISION        AS rate,
             'broker_inverse'                                             AS source
      FROM portfolio_events_log
      WHERE event_type = 'FX_CONVERSION'
        AND ((payload::jsonb) ->> 'rate')::DOUBLE PRECISION > 0;
