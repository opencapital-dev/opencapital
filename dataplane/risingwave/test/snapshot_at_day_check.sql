-- snapshot_at_day_check.sql
-- Verifies that snapshot_at_day(portfolio_id, day) equals the latest fold_per_ts
-- snapshot with business_ts <= day + 1 day (what the ASOF join returns for a
-- close-of-day price tick). Compares equity_positions array length as a stable
-- scalar proxy for the full snapshot.
--
-- NOTE: Use explicit ON join (not USING) — RisingWave's USING on DATE columns
-- can produce a Cartesian product when one side comes from a CTE.
--
-- Expected result: mismatches = 0
WITH fold_per_ts AS (
    SELECT fpe.portfolio_id, fpe.business_ts, fpe.fold_result
    FROM fold_per_event fpe
    JOIN (
        SELECT portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event
        GROUP BY portfolio_id, business_ts
    ) m USING (portfolio_id, business_ts, source_id)
),
days AS (
    SELECT DISTINCT portfolio_id, date_trunc('day', price_ts)::date AS day
    FROM prices
),
expected AS (
    SELECT d.portfolio_id, d.day,
           (SELECT (f.fold_result).snapshot
            FROM fold_per_ts f
            WHERE f.portfolio_id = d.portfolio_id
              AND f.business_ts <= d.day::timestamp + INTERVAL '1 day'
            ORDER BY f.business_ts DESC
            LIMIT 1) AS snap
    FROM days d
)
SELECT count(*) AS mismatches
FROM expected e
JOIN snapshot_at_day s
  ON  s.portfolio_id = e.portfolio_id
  AND s.day          = e.day
WHERE e.snap IS NOT NULL
  AND jsonb_array_length(e.snap -> 'equity_positions')
      <> jsonb_array_length(s.snapshot -> 'equity_positions');
