-- fx_pairs_used — FX-pair backfill discovery feed.
-- One row per (base_ccy, quote_ccy) seen in any foreign-currency TRADE/DIVIDEND.
-- Timestamps are bigint µs (contract: last_seen_ts is the TimeCol).
CREATE MATERIALIZED VIEW IF NOT EXISTS fx_pairs_used AS
    SELECT
        p.base_currency                                                 AS base_ccy,
        UPPER(e.payload ->> 'currency')                                 AS quote_ccy,
        (extract(epoch from MIN(e.business_ts)) * 1000000)::bigint      AS first_seen_ts,
        (extract(epoch from MAX(e.business_ts)) * 1000000)::bigint      AS last_seen_ts,
        (extract(epoch from MAX(e.business_ts)) * 1000000)::bigint      AS ts,
        p.base_currency                                                 AS base,
        UPPER(e.payload ->> 'currency')                                 AS quote,
        COUNT(*)                                                        AS event_count
    FROM events e
    JOIN portfolios p USING (portfolio_id)
    WHERE e.event_type IN ('TRADE', 'DIVIDEND')
      AND e.payload ->> 'currency' IS NOT NULL
      AND UPPER(e.payload ->> 'currency') <> p.base_currency
    GROUP BY p.base_currency, UPPER(e.payload ->> 'currency');
