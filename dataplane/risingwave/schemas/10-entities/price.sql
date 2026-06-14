CREATE VIEW IF NOT EXISTS e_price AS
SELECT
    org_id,
    portfolio_id                                          AS portfolio,
    instrument_id                                         AS instrument,
    (extract(epoch from price_ts) * 1000000)::bigint      AS ts,
    price                                                 AS value,
    currency
FROM prices;
