-- v6 Phase 3+: each Grafana org maps 1:1 to a control-plane org. The
-- Grafana ID JWT carries `aud="org:N"` (Grafana's integer org id);
-- /jwt/mint extracts N and resolves the control-plane org UUID via this
-- column. Grafana itself is configured (auth.generic_oauth org_mapping)
-- to assign users to Grafana orgs based on their Kinde org_code, so the
-- chain is: Kinde org_code -> Grafana org id -> control-plane org_id.

ALTER TABLE organisations ADD COLUMN grafana_org_id INTEGER UNIQUE;

-- Default seeded org corresponds to Grafana's default org (id=1, created
-- automatically on first Grafana boot). Production org creation flow
-- (Phase 7 landing app) assigns grafana_org_id via the Grafana admin API.
UPDATE organisations SET grafana_org_id = 1
 WHERE org_id = '00000000-0000-0000-0000-000000000001';
