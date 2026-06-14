-- Dev-stack seed: a single "default" org plus the membership + install rows
-- the Phase 1 smoke harness needs to mint a JWT. Phase 2/3 replace this with
-- real provisioning via the control plane's /orgs and /orgs/{id}/plugins
-- endpoints; until then this row is the only org in the system.

INSERT INTO organisations (org_id, short_id, name)
VALUES ('00000000-0000-0000-0000-000000000001', '00000000', 'default')
ON CONFLICT (org_id) DO NOTHING;

INSERT INTO user_org (user_id, org_id, role) VALUES
    ('smoke', '00000000-0000-0000-0000-000000000001', 'operator'),
    ('dev',   '00000000-0000-0000-0000-000000000001', 'operator')
ON CONFLICT (user_id, org_id) DO NOTHING;

INSERT INTO plugin_installs (org_id, plugin_id, version) VALUES
    ('00000000-0000-0000-0000-000000000001', 'core-app',    'phase-1'),
    ('00000000-0000-0000-0000-000000000001', 'yfinance-app',  'phase-1')
ON CONFLICT (org_id, plugin_id) DO NOTHING;
