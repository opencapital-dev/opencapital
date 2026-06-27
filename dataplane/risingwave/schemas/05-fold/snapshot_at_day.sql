-- snapshot_at_day — fold snapshot carried onto the daily price calendar.
-- Replaces the per-tick `ASOF fold_per_ts ON (portfolio_id)` join for PRICE/OPTION
-- ticks (amplification source). Event-time ticks keep the exact direct join.
--
-- Columns: portfolio_id, day DATE, snapshot JSONB
-- One row per (portfolio_id, price-tick day): the latest fold snapshot with
-- business_ts <= day + 1 day (close-of-day semantics, matching the ASOF that
-- held_at_tick used). ASOF LEFT JOIN gives "latest at or before" in one pass
-- without the UNION+LAST_VALUE calendar overhead that caused slow backfill on
-- the complex fold_per_event DAG.
SET BACKGROUND_DDL = true;

CREATE MATERIALIZED VIEW IF NOT EXISTS snapshot_at_day AS
WITH fold_per_ts AS (
    -- One row per (portfolio_id, business_ts): latest source_id wins.
    SELECT fpe.portfolio_id, fpe.business_ts, (fpe.fold_result).snapshot AS snapshot
    FROM fold_per_event fpe
    JOIN (
        SELECT portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event
        GROUP BY portfolio_id, business_ts
    ) m USING (portfolio_id, business_ts, source_id)
),
price_days AS (
    -- One row per (portfolio_id, calendar day): distinct price-tick days.
    SELECT DISTINCT
        portfolio_id,
        date_trunc('day', price_ts)::date AS day
    FROM prices
)
SELECT
    pd.portfolio_id,
    pd.day,
    f.snapshot
FROM price_days pd
ASOF LEFT JOIN fold_per_ts f
    ON  f.portfolio_id = pd.portfolio_id
    AND (pd.day::timestamp + INTERVAL '1 day') >= f.business_ts;
