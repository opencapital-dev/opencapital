-- prices_daily_check.sql
-- Asserts that the target relation has at most 1 row per (portfolio, instrument, day).
-- Run with: psql ... -v rel=prices_candidate -f prices_daily_check.sql
-- Expect: max_per_day = 1, total ≈ 11017
SELECT
    (SELECT max(c)
       FROM (SELECT count(*) AS c
               FROM :rel
              GROUP BY portfolio_id, instrument_id, price_ts::date) z) AS max_per_day,
    (SELECT count(*) FROM :rel) AS total;
