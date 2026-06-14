-- v6 Phase 9: migration 0016 reserved grafana_org_id=2 for the test2
-- seed org under the assumption an operator would manually create
-- Grafana org 2 to match. Phase 7 onboarding wizard + Phase 9 admin
-- bootstrap both AUTO-create Grafana orgs via the admin API, where
-- Grafana's next-org-id counter eventually hands out 2 -- collides
-- with the seed row's reservation. UNIQUE(grafana_org_id) on
-- organisations 23505s the wizard.
--
-- Drop the reservation; test2 stays in control_db but unmapped from
-- Grafana. Cross-tenant smoke (Phase 7 verification step 9) can still
-- use the test2 schema directly via psql to RW. If a future operator
-- needs test2 in Grafana, they create the Grafana org manually and
-- update the column.

UPDATE organisations
   SET grafana_org_id = NULL
 WHERE org_id = '00000000-0000-0000-0000-000000000002';
