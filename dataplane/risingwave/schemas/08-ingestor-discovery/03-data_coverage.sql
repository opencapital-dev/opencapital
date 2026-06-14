-- data_coverage: a plugin's newest data point per source across all namespaces.
-- plugin_id injected by read-gateway (plugin sees only its own rows).
-- observed_at as bigint µs; [2000, 2100) guard keeps scale-bug garbage out of
-- @latest without a dynamic now() filter (which would turn this into a stream).
CREATE VIEW data_coverage AS
SELECT org_id,
       plugin_id,
       portfolio_id                                          AS portfolio,
       source_id,
       source_id                                              AS instrument,
       source_namespace,
       (extract(epoch from observed_at) * 1000000)::bigint   AS observed_at,
       (extract(epoch from observed_at) * 1000000)::bigint   AS ts
FROM data_log
WHERE observed_at >= '2000-01-01' AND observed_at < '2100-01-01';
