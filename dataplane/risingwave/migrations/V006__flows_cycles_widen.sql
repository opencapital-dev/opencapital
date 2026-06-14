-- Widen e_flows + e_cycles for the analytics metrics.
--
-- e_flows: the old amt read payload->>'amount' — a field the cashflow payloads
-- do not populate (they use amount_native), so amt was silently 0 on every row.
-- Fix it to the signed cash effect from amount_native + type, matching the
-- import cash convention (sim.ts): DEPOSIT/INTEREST/TRANSFER add cash (+),
-- WITHDRAWAL removes it (-); amount_native is always a positive magnitude with
-- the direction carried by the event type. Add flow_type so callers (xirr) can
-- filter to DEPOSIT/WITHDRAWAL and negate for the investor-POV IRR solve.
--
-- e_cycles: expose pnl_base, duration_sec, and was_re_entry (as 1.0/0.0) for
-- the re-entry counts and the hit-rate-by-duration buckets.
--
-- RisingWave views: DROP + CREATE is safe (the gateway is the only reader; no
-- downstream views depend on these). Dev data is disposable.

DROP VIEW IF EXISTS e_flows;

CREATE VIEW e_flows AS
SELECT
    org_id,
    portfolio_id,
    business_ts                                AS ts,
    COALESCE(payload ->> 'type', event_type)   AS flow_type,
    CASE WHEN (payload ->> 'type') = 'WITHDRAWAL'
         THEN -COALESCE((payload ->> 'amount_native')::DOUBLE PRECISION, 0.0)
         ELSE  COALESCE((payload ->> 'amount_native')::DOUBLE PRECISION, 0.0)
    END                                        AS amt
FROM events
WHERE (event_type = 'CASHFLOW' AND (payload ->> 'type') IN ('DEPOSIT', 'WITHDRAWAL'))
   OR event_type = 'TRANSFER_IN';

DROP VIEW IF EXISTS e_cycles;

CREATE VIEW e_cycles AS
SELECT
    org_id,
    portfolio_id,
    instrument_id,
    close_ts,
    pnl_base,
    duration_sec,
    CASE WHEN was_re_entry THEN 1.0 ELSE 0.0 END  AS was_re_entry
FROM cycles_per_event;
