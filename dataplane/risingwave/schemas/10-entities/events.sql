CREATE VIEW IF NOT EXISTS e_events AS
SELECT
    org_id,
    portfolio_id                                          AS portfolio,
    instrument_id                                         AS instrument,
    (extract(epoch from business_ts) * 1000000)::bigint   AS ts,
    event_type,
    payload,
    base_currency
FROM events;
