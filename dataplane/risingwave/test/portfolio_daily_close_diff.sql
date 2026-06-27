-- Portfolio daily-close diff: compare the last tick of each day in :gold against
-- the corresponding row in :new (which has exactly one row per (scope, day)).
-- Portfolio has NO instrument_id and NO currency — the key is (scope_id, day).
--
-- Usage:
--   psql -h localhost -p 4566 -U root -d dev \
--        -v new=portfolio_per_tick_new \
--        -v gold=gold_portfolio_per_tick \
--        -f portfolio_daily_close_diff.sql

-- ── 1. Mismatch count ────────────────────────────────────────────────────────
WITH gold_close AS (
    SELECT *,
           row_number() OVER (
               PARTITION BY scope_id, event_ts::date
               ORDER BY event_ts DESC
           ) AS rn
    FROM :gold
),
gold_eod AS (
    SELECT * FROM gold_close WHERE rn = 1
)
SELECT 'mismatches' AS check, count(*) AS rows
FROM gold_eod g
JOIN :new n
    ON  n.scope_id       = g.scope_id
    AND n.event_ts::date = g.event_ts::date
WHERE abs(coalesce(n.nav_base,                  0) - coalesce(g.nav_base,                  0)) > 1e-6
   OR abs(coalesce(n.equity_value_base,         0) - coalesce(g.equity_value_base,         0)) > 1e-6
   OR abs(coalesce(n.cash_value_base,           0) - coalesce(g.cash_value_base,           0)) > 1e-6
   OR abs(coalesce(n.total_gross_avg_base,      0) - coalesce(g.total_gross_avg_base,      0)) > 1e-6
   OR abs(coalesce(n.total_net_avg_base,        0) - coalesce(g.total_net_avg_base,        0)) > 1e-6
   OR abs(coalesce(n.realized_equity_avg_base,  0) - coalesce(g.realized_equity_avg_base,  0)) > 1e-6
   OR abs(coalesce(n.realized_forex_avg_base,   0) - coalesce(g.realized_forex_avg_base,   0)) > 1e-6

UNION ALL

-- ── 2. Gold-close keys missing from new ─────────────────────────────────────
SELECT 'gold_missing_from_new' AS check, count(*) AS rows
FROM (
    SELECT scope_id, event_ts::date AS day FROM gold_eod
    EXCEPT
    SELECT scope_id, event_ts::date AS day FROM :new
) missing

UNION ALL

-- ── 3. New keys missing from gold-close ─────────────────────────────────────
SELECT 'new_missing_from_gold' AS check, count(*) AS rows
FROM (
    SELECT scope_id, event_ts::date AS day FROM :new
    EXCEPT
    SELECT scope_id, event_ts::date AS day FROM gold_eod
) extra;
