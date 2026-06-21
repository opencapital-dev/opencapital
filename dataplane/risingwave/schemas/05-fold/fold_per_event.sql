-- ============================================================================
-- fold_per_event — v5 windowed UDAF MV.
-- ----------------------------------------------------------------------------
-- One row per (portfolio_id, business_ts, source_id), with the full
-- running portfolio state as of that event. Replaces v4's fold_output (which
-- emitted a per-portfolio JSONB array and required a lateral-unnest sink).
--
-- Past-row UPSERT/INSERT/DELETE on portfolio_events_log propagates into the
-- events MV → into this windowed aggregation → RisingWave's OverWindow
-- operator re-accumulates the partition tail. No application-level recompute
-- coordinator.
--
-- portfolio_id is the sole scope key. PARTITION BY e.portfolio_id carries
-- the per-portfolio scope into the OverWindow operator.
--
-- Outputs (struct fields, projected for sink convenience):
--   snapshot:  full portfolio state as JSONB.
--   closures:  array of lot closures emitted by THIS event.
--   cycles:    array of cycles closed by THIS event.
--   dirty:     true if retract diverged and the snapshot may be approximate.
-- ============================================================================

DROP MATERIALIZED VIEW IF EXISTS fold_per_event CASCADE;

CREATE MATERIALIZED VIEW fold_per_event AS
SELECT
    e.portfolio_id,
    e.business_ts,
    e.source_id,
    e.event_type,
    e.instrument_id,
    (fold_kernel(e.enriched) OVER (
        PARTITION BY e.portfolio_id
        ORDER BY e.business_ts, e.source_id
        ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
    )) AS fold_result
FROM events e;
