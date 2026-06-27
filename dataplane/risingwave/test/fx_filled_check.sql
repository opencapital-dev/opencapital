-- fx_filled_check.sql
-- For every price-tick day and pair the portfolio uses, the filled rate must
-- equal the latest fx_rates rate <= that day (what the current ASOF returns).
-- instruments has no currency column; use prices.currency as from_ccy.
WITH used AS (
  SELECT DISTINCT px.currency AS from_ccy, p.base_currency AS to_ccy,
         date_trunc('day', px.price_ts)::date AS day
  FROM prices px
  JOIN portfolios p USING (portfolio_id)
  WHERE px.currency IS NOT NULL AND px.currency <> p.base_currency
),
asof_expected AS (
  SELECT u.from_ccy, u.to_ccy, u.day,
         (SELECT r.rate FROM fx_rates r
           WHERE r.from_ccy = u.from_ccy AND r.to_ccy = u.to_ccy
             AND r.ts <= u.day::timestamptz + INTERVAL '1 day'
           ORDER BY r.ts DESC LIMIT 1) AS expected_rate
  FROM used u
)
SELECT count(*) AS mismatches
FROM asof_expected e
JOIN fx_filled f
  ON f.from_ccy = e.from_ccy AND f.to_ccy = e.to_ccy AND f.day = e.day
WHERE e.expected_rate IS NOT NULL
  AND abs(f.rate - e.expected_rate) > 1e-9;
