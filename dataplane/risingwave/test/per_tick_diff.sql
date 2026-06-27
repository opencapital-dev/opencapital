-- Usage: psql ... -v new=instrument_per_tick_new -v gold=gold_instrument_per_tick -f per_tick_diff.sql
-- Strategy: round every DOUBLE PRECISION column to 9 decimals, then EXCEPT both ways.
-- A non-zero count = a real divergence to fix.
SELECT 'new_not_in_gold' AS dir, count(*) AS rows FROM (
  SELECT * FROM (SELECT * FROM :new) n
  EXCEPT
  SELECT * FROM (SELECT * FROM :gold) g
) d
UNION ALL
SELECT 'gold_not_in_new', count(*) FROM (
  SELECT * FROM (SELECT * FROM :gold) g
  EXCEPT
  SELECT * FROM (SELECT * FROM :new) n
) d;

-- drill-down: which columns differ for a given key
-- SELECT n.scope_id, n.instrument_id, n.event_ts,
--        n.equity_value_base, g.equity_value_base
-- FROM :new n JOIN :gold g USING (scope_type, scope_id, instrument_id, event_ts)
-- WHERE abs(coalesce(n.equity_value_base,0) - coalesce(g.equity_value_base,0)) > 1e-9;
