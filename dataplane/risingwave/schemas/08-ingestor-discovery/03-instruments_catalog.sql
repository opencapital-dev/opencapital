-- instruments_catalog — instruments held in a portfolio with ref attributes.
-- portfolio_id kept alongside portfolio: discovery.go and handlers_ref.go read
-- it as data from result rows to correlate back to the job/request portfolio.
-- instrument_id kept alongside instrument: discovery.go:85 and
-- handlers_ref.go:183 read it as data from result rows.
-- last_seen_ts is bigint µs (TimeCol for read-gateway @latest / @window).
CREATE VIEW instruments_catalog AS
SELECT u.org_id,
       u.portfolio_id                                          AS portfolio,
       u.portfolio_id                                          AS portfolio_id,
       u.instrument_id                                         AS instrument,
       u.instrument_id                                         AS instrument_id,
       i.kind,
       i.contract_multiplier,
       ccy.currency,
       ccy.base_currency,
       (extract(epoch from u.first_seen_ts) * 1000000)::bigint AS first_seen_ts,
       (extract(epoch from u.last_seen_ts)  * 1000000)::bigint AS last_seen_ts,
       (extract(epoch from u.last_seen_ts)  * 1000000)::bigint AS ts,
       u.event_count
FROM instruments_used u
LEFT JOIN instruments i
  ON i.org_id = u.org_id
 AND i.portfolio_id = u.portfolio_id
 AND i.instrument_id = u.instrument_id
LEFT JOIN (
    SELECT DISTINCT org_id, scope_id AS portfolio_id, instrument_id, currency, base_currency
    FROM instrument_per_event
) ccy
  ON ccy.org_id = u.org_id
 AND ccy.portfolio_id = u.portfolio_id
 AND ccy.instrument_id = u.instrument_id;
