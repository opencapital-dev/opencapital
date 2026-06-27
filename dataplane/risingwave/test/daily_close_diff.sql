-- Daily-close diff: compare the last tick of each day in :gold against the
-- corresponding row in :new (which has exactly one row per instrument per day).
--
-- Usage:
--   psql -h localhost -p 4566 -U root -d dev \
--        -v new=instrument_per_tick_new \
--        -v gold=gold_instrument_per_tick \
--        -f daily_close_diff.sql

-- ── 1. Mismatch count ────────────────────────────────────────────────────────
WITH gold_close AS (
    SELECT *,
           row_number() OVER (
               PARTITION BY scope_id, instrument_id, event_ts::date
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
    ON  n.scope_id                      = g.scope_id
    AND coalesce(n.instrument_id, '')   = coalesce(g.instrument_id, '')
    AND n.event_ts::date                = g.event_ts::date
WHERE abs(coalesce(n.equity_value_base,           0) - coalesce(g.equity_value_base,           0)) > 1e-6
   OR abs(coalesce(n.unrealized_equity_fifo_base, 0) - coalesce(g.unrealized_equity_fifo_base, 0)) > 1e-6
   OR abs(coalesce(n.unrealized_equity_avg_base,  0) - coalesce(g.unrealized_equity_avg_base,  0)) > 1e-6
   OR abs(coalesce(n.last_price,                  0) - coalesce(g.last_price,                  0)) > 1e-6
   OR abs(coalesce(n.quantity,                    0) - coalesce(g.quantity,                    0)) > 1e-6

UNION ALL

-- ── 2. Gold-close keys missing from new ─────────────────────────────────────
SELECT 'gold_missing_from_new' AS check, count(*) AS rows
FROM (
    SELECT scope_id, instrument_id, event_ts::date AS day FROM gold_eod
    EXCEPT
    SELECT scope_id, instrument_id, event_ts::date AS day FROM :new
) missing

UNION ALL

-- ── 3. New keys missing from gold-close ─────────────────────────────────────
SELECT 'new_missing_from_gold' AS check, count(*) AS rows
FROM (
    SELECT scope_id, instrument_id, event_ts::date AS day FROM :new
    EXCEPT
    SELECT scope_id, instrument_id, event_ts::date AS day FROM gold_eod
) extra;
