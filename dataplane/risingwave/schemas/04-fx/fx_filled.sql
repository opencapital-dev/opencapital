-- ============================================================================
-- fx_filled — daily, forward-filled FX rate keyed on (from_ccy, to_ccy, day).
-- ----------------------------------------------------------------------------
-- Replaces the per-tick ASOF fx_rates ON (pair) join that amplifies ~17k
-- rows/update on each import and wedges the streaming engine's barriers.
--
-- Calendar domain = days that actually carry ticks needing a rate:
--   * price-tick days for all pairs whose instruments are priced in a
--     currency different from the portfolio's base_currency
--   * fx-event days (days fx_rates itself has a rate)
-- This keeps fx_filled strictly bounded (no open-ended generate_series).
--
-- Correctness: fx_filled(from_ccy, to_ccy, day).rate equals the ASOF rate
-- that instrument_per_tick would pick: latest fx_rates.rate with ts <= day+1d.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS fx_filled AS
WITH daily_rate AS (
    -- Last broker rate within each (from_ccy, to_ccy, calendar day).
    -- row_number() arg-max: pick the row with the latest ts per day.
    SELECT from_ccy, to_ccy, day, rate
    FROM (
        SELECT from_ccy, to_ccy,
               date_trunc('day', ts)::date AS day,
               rate,
               row_number() OVER (
                   PARTITION BY from_ccy, to_ccy, date_trunc('day', ts)::date
                   ORDER BY ts DESC
               ) AS rn
        FROM fx_rates
    ) t
    WHERE rn = 1
),
tick_days AS (
    -- Distinct (pair, day) that a per-tick MV will request a rate for.
    -- instruments has no currency column; prices.currency is the native currency.
    SELECT DISTINCT px.currency AS from_ccy, p.base_currency AS to_ccy,
           date_trunc('day', px.price_ts)::date AS day
    FROM prices px
    JOIN portfolios p USING (portfolio_id)
    WHERE px.currency IS NOT NULL AND px.currency <> p.base_currency
    UNION
    -- Also include fx-event days so forward-fill anchors on every known rate.
    SELECT DISTINCT from_ccy, to_ccy, day FROM daily_rate
),
calendar AS (
    -- Left-join tick_days to daily_rate: NULL rate where no rate fell on that day.
    SELECT td.from_ccy, td.to_ccy, td.day, dr.rate
    FROM tick_days td
    LEFT JOIN daily_rate dr
        ON  dr.from_ccy = td.from_ccy
        AND dr.to_ccy   = td.to_ccy
        AND dr.day      = td.day
)
SELECT from_ccy, to_ccy, day,
       LAST_VALUE(rate IGNORE NULLS) OVER (
           PARTITION BY from_ccy, to_ccy
           ORDER BY day
           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
       ) AS rate
FROM calendar;
