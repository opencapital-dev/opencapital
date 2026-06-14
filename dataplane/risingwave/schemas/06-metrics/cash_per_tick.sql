-- ============================================================================
-- cash_per_tick — dense per-(portfolio, currency, tick) cash MV.
-- ----------------------------------------------------------------------------
-- v5 simplification: reads `fold_per_event` directly (no snapshots
-- durable table in between). `cash_state` is built from
-- `(fold_result).snapshot.cash_positions` with ASOF fx_rates at the
-- per-event business_ts. The outer MV ASOF-joins fx_rates again at the
-- tick to mark cash_value_base + unrealized_fx_avg_base at the latest FX.
--
-- Tick grid: every FX tick for currency→base_currency UNION every cash
-- event_ts.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS cash_per_tick AS
WITH fold_per_ts AS (
    -- One fold row per (org_id, portfolio_id, business_ts): max source_id
    -- wins. Prevents the lateral over cash_positions from double-counting
    -- when multiple events share a business_ts. v6 keys the collapse on
    -- org_id too so colliding portfolio_ids across orgs do not race.
    SELECT fpe.org_id, fpe.portfolio_id, fpe.business_ts, fpe.fold_result
    FROM fold_per_event fpe
    JOIN (
        SELECT org_id, portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event
        GROUP BY org_id, portfolio_id, business_ts
    ) m USING (org_id, portfolio_id, business_ts, source_id)
),
cash_state AS (
    SELECT
        u.org_id                                               AS org_id,
        u.portfolio_id                                         AS scope_id,
        (u.cp ->> 'currency')                                  AS currency,
        (u.portfolio_core ->> 'base_currency')                 AS base_currency,
        u.business_ts                                          AS event_ts,
        (u.cp ->> 'cash_value_native')::DOUBLE PRECISION       AS balance_native,
        (u.cp ->> 'cash_value_native')::DOUBLE PRECISION       AS cash_value_native,
        (u.cp ->> 'cash_value_native')::DOUBLE PRECISION
            * (CASE WHEN (u.cp ->> 'currency')
                       = (u.portfolio_core ->> 'base_currency')
                    THEN 1.0 ELSE fx.rate END)                 AS cash_value_base,
        (u.cp ->> 'realized_interest_native')::DOUBLE PRECISION  AS realized_interest_native,
        (u.cp ->> 'realized_interest_base')::DOUBLE PRECISION    AS realized_interest_base,
        (u.cp ->> 'realized_dividends_native')::DOUBLE PRECISION AS realized_dividends_native,
        (u.cp ->> 'realized_dividends_base')::DOUBLE PRECISION   AS realized_dividends_base,
        (u.cp ->> 'realized_fx_fifo_base')::DOUBLE PRECISION   AS realized_fx_fifo_base,
        (u.cp ->> 'realized_fx_avg_base')::DOUBLE PRECISION    AS realized_fx_avg_base,
        (u.cp ->> 'fees_native')::DOUBLE PRECISION             AS fees_native,
        (u.cp ->> 'fees_base')::DOUBLE PRECISION               AS fees_base,
        (u.cp ->> 'deposits_native')::DOUBLE PRECISION         AS deposits_cumulative_native,
        (u.cp ->> 'withdrawals_native')::DOUBLE PRECISION      AS withdrawals_cumulative_native
    FROM (
        SELECT
            fpe.org_id,
            fpe.portfolio_id,
            fpe.business_ts,
            (fpe.fold_result).snapshot -> 'portfolio_core' AS portfolio_core,
            cp
        FROM fold_per_ts fpe,
             jsonb_array_elements((fpe.fold_result).snapshot -> 'cash_positions') AS cp
    ) u
    ASOF LEFT JOIN fx_rates fx
        ON  fx.org_id   = u.org_id
        AND fx.from_ccy = (u.cp ->> 'currency')
        AND fx.to_ccy   = (u.portfolio_core ->> 'base_currency')
        AND u.business_ts >= fx.ts
)
SELECT
    with_state.org_id                                 AS org_id,
    'portfolio'                                       AS scope_type,
    with_state.scope_id,
    with_state.currency,
    with_state.base_currency,
    with_state.tick_ts                                AS event_ts,
    with_state.balance_native,
    with_state.cash_value_native,
    with_state.balance_native
        * (CASE WHEN with_state.currency = with_state.base_currency
                THEN 1.0 ELSE fx.rate END)            AS cash_value_base,
    with_state.realized_interest_native,
    with_state.realized_interest_base,
    with_state.realized_dividends_native,
    with_state.realized_dividends_base,
    with_state.realized_fx_fifo_base,
    with_state.realized_fx_avg_base,
    with_state.fees_native,
    with_state.fees_base,
    with_state.deposits_cumulative_native,
    with_state.withdrawals_cumulative_native,
    CASE WHEN with_state.currency = with_state.base_currency THEN 0.0
         ELSE with_state.balance_native
              * (fx.rate
                 - COALESCE(with_state.cash_value_base
                            / NULLIF(with_state.cash_value_native, 0),
                            fx.rate))
    END                                               AS unrealized_fx_avg_base
FROM (
    SELECT
        sc.org_id, sc.scope_id, sc.currency, sc.base_currency,
        t.tick_ts,
        cs.balance_native,
        cs.cash_value_native,
        cs.cash_value_base,
        cs.realized_interest_native,
        cs.realized_interest_base,
        cs.realized_dividends_native,
        cs.realized_dividends_base,
        cs.realized_fx_fifo_base,
        cs.realized_fx_avg_base,
        cs.fees_native,
        cs.fees_base,
        cs.deposits_cumulative_native,
        cs.withdrawals_cumulative_native
    FROM (SELECT DISTINCT org_id, scope_id, currency, base_currency FROM cash_state) sc
    JOIN (
        -- Tick grid carries org_id so the per-tick fx_rates ASOF below
        -- and the cash_state ASOF stay org-scoped.
        SELECT org_id, from_ccy AS currency, to_ccy AS base_currency, ts AS tick_ts
        FROM fx_rates
        UNION ALL
        SELECT DISTINCT org_id, currency, base_currency, event_ts AS tick_ts
        FROM cash_state
    ) t
        ON t.org_id = sc.org_id
       AND t.currency = sc.currency
       AND t.base_currency = sc.base_currency
    ASOF LEFT JOIN cash_state cs
        ON  cs.org_id   = sc.org_id
        AND cs.scope_id = sc.scope_id
        AND cs.currency = sc.currency
        AND t.tick_ts   >= cs.event_ts
) with_state
ASOF LEFT JOIN fx_rates fx
    ON  fx.org_id   = with_state.org_id
    AND fx.from_ccy = with_state.currency
    AND fx.to_ccy   = with_state.base_currency
    AND with_state.tick_ts >= fx.ts
WHERE with_state.balance_native IS NOT NULL;
