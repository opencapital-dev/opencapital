-- ============================================================================
-- cycles_per_event — per-event cycle closures projected from fold_per_event.
-- ----------------------------------------------------------------------------
-- v5: replaces the durable `cycles` table + s_cycles sink. Reads
-- `fold_per_event.fold_result.cycles` directly. One row per closed cycle
-- (typically 0 or 1 per event — the closing sell takes the position flat).
--
-- Column shape matches the v4 `cycles` table so existing Numbat /
-- Grafana queries keep working with a one-line FROM rename.
--
-- `was_re_entry` is currently hard-coded false; the UDAF tracks
-- `cycle_count` but not a per-cycle "was this the 2nd+ open" flag.
-- Restore that bit by checking cycle_seq > 0 (since cycle_seq=0 means
-- first cycle on this instrument, and any later cycle is a re-entry).
-- ============================================================================

CREATE MATERIALIZED VIEW IF NOT EXISTS cycles_per_event AS
SELECT
    fpe.portfolio_id                                       AS portfolio_id,
    fpe.instrument_id                                      AS instrument_id,
    (c ->> 'cycle_seq')::INT                               AS cycle_seq,
    (c ->> 'open_ts')::TIMESTAMPTZ                         AS open_ts,
    (c ->> 'close_ts')::TIMESTAMPTZ                        AS close_ts,
    (c ->> 'pnl_base')::DOUBLE PRECISION                   AS pnl_base,
    (c ->> 'duration_sec')::DOUBLE PRECISION               AS duration_sec,
    ((c ->> 'cycle_seq')::INT > 0)                         AS was_re_entry
FROM fold_per_event fpe,
     jsonb_array_elements((fpe.fold_result).cycles) AS t(c);
