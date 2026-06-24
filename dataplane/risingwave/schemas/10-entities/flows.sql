-- amt: WITHDRAWAL is negative (investor POV); DEPOSIT/INTEREST/TRANSFER_IN positive.
-- amount_native is always a positive magnitude; direction is in the event type.
CREATE VIEW IF NOT EXISTS e_flows AS
SELECT
    portfolio_id                                            AS portfolio,
    (extract(epoch from business_ts) * 1000000)::bigint     AS ts,
    COALESCE(payload ->> 'type', event_type)                AS flow_type,
    CASE WHEN (payload ->> 'type') = 'WITHDRAWAL'
         THEN -COALESCE((payload ->> 'amount_native')::DOUBLE PRECISION, 0.0)
         ELSE  COALESCE((payload ->> 'amount_native')::DOUBLE PRECISION, 0.0)
    END                                                     AS amt
FROM events
WHERE (event_type = 'CASHFLOW' AND (payload ->> 'type') IN ('DEPOSIT', 'WITHDRAWAL'))
   OR event_type = 'TRANSFER_IN';
