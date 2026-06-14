CREATE VIEW IF NOT EXISTS e_nav AS
SELECT
    org_id,
    scope_id                                          AS portfolio,
    (extract(epoch from event_ts) * 1000000)::bigint  AS ts,
    nav_base                                          AS value
FROM portfolio_per_tick
WHERE nav_base > 0;
