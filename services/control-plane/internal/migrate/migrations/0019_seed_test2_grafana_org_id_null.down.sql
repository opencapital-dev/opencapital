-- Reverse only if grafana_org_id=2 is still unused. If a real Grafana
-- org has claimed id=2 in the meantime this restore would corrupt the
-- mapping; leave as no-op in that case.
UPDATE organisations
   SET grafana_org_id = 2
 WHERE org_id = '00000000-0000-0000-0000-000000000002'
   AND NOT EXISTS (SELECT 1 FROM organisations WHERE grafana_org_id = 2);
