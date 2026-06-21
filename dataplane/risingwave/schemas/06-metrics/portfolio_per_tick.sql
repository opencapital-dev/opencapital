-- ============================================================================
-- portfolio_per_tick — dense per-(portfolio, tick) NAV rollup.
-- ----------------------------------------------------------------------------
-- v5 rewrite: held-at-tick semantics. Reads `fold_per_event` directly as
-- the source of truth. Tick grid: every fold event business_ts (so
-- cash-only events appear) UNION every price tick for any instrument
-- the portfolio has ever held. At each tick the snapshot at-or-before
-- tick_ts is ASOF-resolved; equity_positions is lateral-unnested and
-- filtered to qty>0 (closed positions are absent from the array → no
-- phantom rows). Each held position is then ASOF-joined to prices,
-- option_marks, and fx_rates at tick_ts for MtM. Aggregation produces
-- one row per (scope_id, tick_ts).
--
-- Cash is aggregated separately from `(fold_result).snapshot.cash_positions`
-- with event-time FX (matches v1 portfolio_metrics_portfolio_dense).
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS portfolio_per_tick AS
WITH fold_per_ts AS (
    -- One row per (portfolio_id, business_ts): the fold_per_event
    -- row with the largest source_id at that business_ts. The OverWindow
    -- operator orders within a business_ts by source_id, so the
    -- max-source_id row carries the latest cumulative state. Pre-filtering
    -- here prevents double-counting when multiple events share a
    -- business_ts.
    SELECT fpe.portfolio_id, fpe.business_ts, fpe.source_id, fpe.fold_result
    FROM fold_per_event fpe
    JOIN (
        SELECT portfolio_id, business_ts, MAX(source_id) AS source_id
        FROM fold_per_event
        GROUP BY portfolio_id, business_ts
    ) m USING (portfolio_id, business_ts, source_id)
),
portfolio_instruments AS (
    -- All (portfolio, instrument) pairs the fold has ever seen with qty>0.
    SELECT DISTINCT
        fpe.portfolio_id,
        (ep ->> 'instrument_id') AS instrument_id
    FROM fold_per_ts fpe,
         jsonb_array_elements((fpe.fold_result).snapshot -> 'equity_positions') AS t(ep)
),
portfolio_tick_grid AS (
    -- Per-portfolio ticks: union of price ticks for the portfolio's
    -- ever-held instruments + every fold event business_ts (for cash-only
    -- events). DISTINCT keeps the cardinality bounded.
    SELECT DISTINCT pi.portfolio_id AS scope_id, px.price_ts AS tick_ts
      FROM portfolio_instruments pi
      JOIN prices px
        ON  px.portfolio_id  = pi.portfolio_id
        AND px.instrument_id = pi.instrument_id
    UNION
    SELECT DISTINCT pi.portfolio_id AS scope_id, om.price_ts AS tick_ts
      FROM portfolio_instruments pi
      JOIN option_marks om
        ON  om.instrument_id = pi.instrument_id
    UNION
    SELECT DISTINCT portfolio_id AS scope_id, business_ts AS tick_ts
      FROM fold_per_ts
),
held_at_tick AS (
    -- ASOF the latest snapshot at-or-before tick_ts.
    SELECT
        t.scope_id, t.tick_ts,
        (fpe.fold_result).snapshot AS snap
    FROM portfolio_tick_grid t
    ASOF LEFT JOIN fold_per_ts fpe
        ON  fpe.portfolio_id = t.scope_id
        AND t.tick_ts >= fpe.business_ts
),
positions_at_tick AS (
    -- Lateral-unnest equity_positions; filter qty>0 (closed positions
    -- already absent from the array under v5 fold semantics — this is
    -- belt-and-suspenders).
    SELECT
        h.scope_id, h.tick_ts,
        (ep ->> 'instrument_id')                                  AS instrument_id,
        (ep ->> 'quantity')::DOUBLE PRECISION                     AS quantity,
        (ep ->> 'currency')                                       AS currency,
        (ep ->> 'base_currency')                                  AS base_currency,
        (ep ->> 'avg_cost_fifo_native')::DOUBLE PRECISION         AS avg_cost_fifo_native,
        (ep ->> 'avg_cost_avg_native')::DOUBLE PRECISION          AS avg_cost_avg_native,
        (ep ->> 'avg_cost_fifo_base')::DOUBLE PRECISION           AS avg_cost_fifo_base,
        (ep ->> 'avg_cost_avg_base')::DOUBLE PRECISION            AS avg_cost_avg_base,
        (ep ->> 'realized_equity_fifo_base')::DOUBLE PRECISION    AS realized_equity_fifo_base,
        (ep ->> 'realized_equity_avg_base')::DOUBLE PRECISION     AS realized_equity_avg_base,
        (ep ->> 'realized_forex_fifo_base')::DOUBLE PRECISION     AS realized_forex_fifo_base,
        (ep ->> 'realized_forex_avg_base')::DOUBLE PRECISION      AS realized_forex_avg_base
    FROM held_at_tick h,
         jsonb_array_elements(h.snap -> 'equity_positions') AS t(ep)
    WHERE (ep ->> 'quantity')::DOUBLE PRECISION > 0
),
positions_priced AS (
    -- ASOF prices + fx at tick_ts for MtM.
    SELECT
        p.*,
        i.kind                                                    AS kind,
        COALESCE(i.contract_multiplier, 1.0)                      AS contract_multiplier,
        CASE WHEN i.kind = 'option' THEN om.close ELSE px.price END AS last_price,
        fx.rate                                                   AS fx_rate
    FROM positions_at_tick p
    LEFT JOIN instruments i
        ON  i.portfolio_id  = p.scope_id
        AND i.instrument_id = p.instrument_id
    ASOF LEFT JOIN prices px
        ON  px.portfolio_id  = p.scope_id
        AND px.instrument_id = p.instrument_id
        AND p.tick_ts >= px.price_ts
    ASOF LEFT JOIN option_marks om
        ON  om.portfolio_id  = p.scope_id
        AND om.instrument_id = p.instrument_id
        AND p.tick_ts >= om.price_ts
    ASOF LEFT JOIN fx_rates fx
        ON  fx.from_ccy = p.currency
        AND fx.to_ccy   = p.base_currency
        AND p.tick_ts >= fx.ts
),
equity_agg AS (
    -- Per-tick aggregation of currently-held positions: live MtM
    -- (equity_value_base + unrealized_*) summed across what's held at
    -- tick_ts. Realized totals are NOT summed here — they live in
    -- portfolio_core (which includes contributions from closed
    -- positions too) and are sourced via the core_at_tick CTE below.
    SELECT
        scope_id, tick_ts,
        SUM(quantity
            * COALESCE(last_price, avg_cost_avg_native)
            * contract_multiplier
            * (CASE WHEN currency = base_currency THEN 1.0 ELSE fx_rate END)
        ) AS equity_value_base,
        SUM(quantity * (COALESCE(last_price, avg_cost_avg_native) - avg_cost_fifo_native)
            * contract_multiplier
            * (CASE WHEN currency = base_currency THEN 1.0 ELSE fx_rate END)
        ) AS unrealized_equity_fifo_base,
        SUM(quantity * (COALESCE(last_price, avg_cost_avg_native) - avg_cost_avg_native)
            * contract_multiplier
            * (CASE WHEN currency = base_currency THEN 1.0 ELSE fx_rate END)
        ) AS unrealized_equity_avg_base,
        SUM(quantity * (avg_cost_fifo_native
                        * (CASE WHEN currency = base_currency THEN 1.0 ELSE fx_rate END)
                        - avg_cost_fifo_base) * contract_multiplier
        ) AS unrealized_forex_fifo_base,
        SUM(quantity * (avg_cost_avg_native
                        * (CASE WHEN currency = base_currency THEN 1.0 ELSE fx_rate END)
                        - avg_cost_avg_base) * contract_multiplier
        ) AS unrealized_forex_avg_base,
        MAX(base_currency)               AS base_currency,
        COUNT(*)                         AS instrument_count
    FROM positions_priced
    GROUP BY scope_id, tick_ts
),
core_at_tick AS (
    -- ASOF latest portfolio_core at-or-before each tick. portfolio_core
    -- rolls up realized PnL (equity + forex + interest + dividends + fees)
    -- across ALL events — including closed positions. The held-positions
    -- aggregation in equity_agg cannot reproduce these because closed
    -- positions are absent from equity_positions.
    SELECT
        t.scope_id, t.tick_ts,
        ((fpe.fold_result).snapshot -> 'portfolio_core') AS core
    FROM portfolio_tick_grid t
    ASOF LEFT JOIN fold_per_ts fpe
        ON  fpe.portfolio_id = t.scope_id
        AND t.tick_ts >= fpe.business_ts
),
cash_per_event AS (
    SELECT
        u.portfolio_id                                AS scope_id,
        u.business_ts                                 AS event_ts,
        MAX(u.portfolio_core ->> 'base_currency')     AS base_currency,
        SUM(
            (u.cp ->> 'cash_value_native')::DOUBLE PRECISION
            * (CASE WHEN (u.cp ->> 'currency')
                       = (u.portfolio_core ->> 'base_currency')
                    THEN 1.0 ELSE fx.rate END)
        )                                             AS cash_value_base,
        SUM((u.cp ->> 'realized_interest_base')::DOUBLE PRECISION)    AS realized_interest_base,
        SUM((u.cp ->> 'realized_dividends_base')::DOUBLE PRECISION)   AS realized_dividends_base,
        SUM((u.cp ->> 'fees_base')::DOUBLE PRECISION)                 AS fees_base,
        SUM((u.cp ->> 'realized_fx_fifo_base')::DOUBLE PRECISION)     AS realized_fx_fifo_base,
        SUM((u.cp ->> 'realized_fx_avg_base')::DOUBLE PRECISION)      AS realized_fx_avg_base,
        SUM((u.cp ->> 'unrealized_fx_avg_base')::DOUBLE PRECISION)    AS unrealized_fx_avg_base,
        COUNT(*)                                                      AS cash_position_count
    FROM (
        SELECT
            fpe.portfolio_id,
            fpe.business_ts,
            (fpe.fold_result).snapshot -> 'portfolio_core' AS portfolio_core,
            cp
        FROM fold_per_ts fpe,
             jsonb_array_elements((fpe.fold_result).snapshot -> 'cash_positions') AS cp
    ) u
    ASOF LEFT JOIN fx_rates fx
        ON  fx.from_ccy = (u.cp ->> 'currency')
        AND fx.to_ccy   = (u.portfolio_core ->> 'base_currency')
        AND u.business_ts >= fx.ts
    GROUP BY u.portfolio_id, u.business_ts
)
SELECT
    'portfolio'                                       AS scope_type,
    eq.scope_id                                       AS scope_id,
    eq.tick_ts                                        AS event_ts,
    COALESCE(eq.base_currency, cash.base_currency)    AS base_currency,
    COALESCE(cash.cash_value_base, 0.0)               AS cash_value_base,
    eq.equity_value_base                              AS equity_value_base,
    COALESCE(cash.cash_value_base, 0.0)
        + COALESCE(eq.equity_value_base, 0.0)         AS nav_base,
    -- Realized fields sourced from portfolio_core (includes closed
    -- positions, which are absent from equity_positions).
    COALESCE((core.core ->> 'realized_equity_fifo_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_equity_fifo_base,
    COALESCE((core.core ->> 'realized_equity_avg_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_equity_avg_base,
    COALESCE((core.core ->> 'realized_forex_fifo_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_forex_fifo_base,
    COALESCE((core.core ->> 'realized_forex_avg_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_forex_avg_base,
    COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_interest_base,
    COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
                                                      AS realized_dividends_base,
    eq.unrealized_equity_fifo_base                    AS unrealized_equity_fifo_base,
    eq.unrealized_equity_avg_base                     AS unrealized_equity_avg_base,
    eq.unrealized_forex_fifo_base + COALESCE(cash.unrealized_fx_avg_base, 0.0)
                                                      AS unrealized_forex_fifo_base,
    eq.unrealized_forex_avg_base  + COALESCE(cash.unrealized_fx_avg_base, 0.0)
                                                      AS unrealized_forex_avg_base,
    COALESCE((core.core ->> 'fees_base')::DOUBLE PRECISION, 0.0) AS fees_base,
    -- total_*_base: realized + unrealized + interest + dividends (gross),
    -- minus fees (net). Provided in two cost-basis variants (fifo, avg).
    -- Realized fields come from portfolio_core (closed positions included).
    (COALESCE((core.core ->> 'realized_equity_fifo_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_forex_fifo_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
        + COALESCE(eq.unrealized_equity_fifo_base, 0.0)
        + COALESCE(eq.unrealized_forex_fifo_base, 0.0)
        + COALESCE(cash.unrealized_fx_avg_base, 0.0)) AS total_gross_fifo_base,
    (COALESCE((core.core ->> 'realized_equity_avg_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_forex_avg_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
        + COALESCE(eq.unrealized_equity_avg_base, 0.0)
        + COALESCE(eq.unrealized_forex_avg_base, 0.0)
        + COALESCE(cash.unrealized_fx_avg_base, 0.0)) AS total_gross_avg_base,
    (COALESCE((core.core ->> 'realized_equity_fifo_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_forex_fifo_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
        + COALESCE(eq.unrealized_equity_fifo_base, 0.0)
        + COALESCE(eq.unrealized_forex_fifo_base, 0.0)
        + COALESCE(cash.unrealized_fx_avg_base, 0.0)
        - COALESCE((core.core ->> 'fees_base')::DOUBLE PRECISION, 0.0))              AS total_net_fifo_base,
    (COALESCE((core.core ->> 'realized_equity_avg_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_forex_avg_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_interest_base')::DOUBLE PRECISION, 0.0)
        + COALESCE((core.core ->> 'realized_dividends_base')::DOUBLE PRECISION, 0.0)
        + COALESCE(eq.unrealized_equity_avg_base, 0.0)
        + COALESCE(eq.unrealized_forex_avg_base, 0.0)
        + COALESCE(cash.unrealized_fx_avg_base, 0.0)
        - COALESCE((core.core ->> 'fees_base')::DOUBLE PRECISION, 0.0))              AS total_net_avg_base,
    eq.instrument_count                               AS instrument_count,
    COALESCE(cash.cash_position_count, 0)             AS cash_position_count
FROM equity_agg eq
LEFT JOIN core_at_tick core
    ON  core.scope_id = eq.scope_id
    AND core.tick_ts  = eq.tick_ts
ASOF LEFT JOIN cash_per_event cash
    ON  cash.scope_id = eq.scope_id
    AND eq.tick_ts >= cash.event_ts;
