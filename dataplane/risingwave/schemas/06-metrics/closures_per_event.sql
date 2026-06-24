-- ============================================================================
-- closures_per_event — per-event lot closures projected from fold_per_event.
-- ----------------------------------------------------------------------------
-- v5: replaces the durable `closures` table + s_closures sink. Reads
-- `fold_per_event.fold_result.closures` directly and lateral-unnests the
-- per-event short array (0–N items per event).
--
-- Column shape matches the v4 `closures` table so existing Numbat /
-- Grafana queries (`lib-metrics/.../closures_binding.sql.snippet`,
-- `instruments_breakdown.sql`) keep working with a one-line FROM rename.
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS closures_per_event AS
SELECT
    fpe.portfolio_id                                       AS portfolio_id,
    fpe.instrument_id                                      AS instrument_id,
    (c ->> 'lot_id')                                       AS lot_id,
    (c ->> 'exit_ts')::TIMESTAMPTZ                         AS exit_ts,
    'long'                                                 AS direction,
    (c ->> 'entry_ts')::TIMESTAMPTZ                        AS entry_ts,
    (c ->> 'quantity')::DOUBLE PRECISION                   AS qty,
    (c ->> 'entry_price')::DOUBLE PRECISION                AS entry_price,
    ((c ->> 'entry_price')::DOUBLE PRECISION
     * (c ->> 'fx_at_buy')::DOUBLE PRECISION)              AS entry_price_base,
    (c ->> 'fx_at_buy')::DOUBLE PRECISION                  AS entry_fx_to_base,
    (c ->> 'exit_price')::DOUBLE PRECISION                 AS exit_price,
    ((c ->> 'exit_price')::DOUBLE PRECISION
     * (c ->> 'fx_at_sell')::DOUBLE PRECISION)             AS exit_price_base,
    (c ->> 'fx_at_sell')::DOUBLE PRECISION                 AS exit_fx_to_base,
    (c ->> 'realized_pnl_native')::DOUBLE PRECISION        AS realized_pnl_native,
    (c ->> 'realized_pnl_base')::DOUBLE PRECISION          AS realized_pnl_base
FROM fold_per_event fpe,
     jsonb_array_elements((fpe.fold_result).closures) AS t(c);
