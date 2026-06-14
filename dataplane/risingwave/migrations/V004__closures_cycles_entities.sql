-- Entity-view layer: closures + cycles over per-event MVs.
-- Idempotent (CREATE VIEW IF NOT EXISTS).

-- ---- closures ------------------------------------------------------------
-- Thin projection over closures_per_event. Aliases:
--   realized_pnl     <- realized_pnl_base
--   holding_seconds  <- extract(epoch from (exit_ts - entry_ts))
CREATE VIEW IF NOT EXISTS e_closures AS
SELECT
    org_id,
    portfolio_id,
    instrument_id,
    exit_ts,
    realized_pnl_base                             AS realized_pnl,
    extract(epoch from (exit_ts - entry_ts))      AS holding_seconds
FROM closures_per_event;

-- ---- cycles --------------------------------------------------------------
-- Thin projection over cycles_per_event.
CREATE VIEW IF NOT EXISTS e_cycles AS
SELECT
    org_id,
    portfolio_id,
    instrument_id,
    close_ts
FROM cycles_per_event;
