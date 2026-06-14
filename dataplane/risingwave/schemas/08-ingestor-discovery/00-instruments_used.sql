-- ============================================================================
-- instruments_used — equity-instrument backfill discovery feed.
-- ----------------------------------------------------------------------------
-- Aggregates from `events` (= portfolio_events): one row per
-- (portfolio, equity instrument) seen in any TRADE / TRANSFER_IN / DIVIDEND.
-- CASH:* and FX:* pseudo-instruments are filtered out.
--
-- Consumed by yfinance-ingestor via `sub_instruments_used` to plan price
-- backfills (start/end ts, instrument list).
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS instruments_used AS
    SELECT
        org_id,
        portfolio_id,
        instrument_id,
        MIN(business_ts)   AS first_seen_ts,
        MAX(business_ts)   AS last_seen_ts,
        COUNT(*)           AS event_count
    FROM events
    WHERE instrument_id IS NOT NULL
      AND instrument_id NOT LIKE 'CASH:%'
      AND instrument_id NOT LIKE 'FX:%'
      AND event_type IN ('TRADE', 'TRANSFER_IN', 'DIVIDEND')
    GROUP BY org_id, portfolio_id, instrument_id;
