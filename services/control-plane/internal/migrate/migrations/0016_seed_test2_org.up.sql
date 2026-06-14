-- v6 Phase 7: seed a second org so cross-tenant denial can be smoke-tested
-- end-to-end (`SELECT count(*) FROM schema_org_<test2>.portfolio_per_tick_v`
-- as the default-org plugin role must fail with "permission denied for
-- schema schema_org_<test2>"; the converse must succeed).
--
-- short_id matches the org's leading 8 hex chars (same convention as the
-- default org's '00000000'). The Grafana org mapping is set so a future
-- Grafana org id=2 (created by the operator) maps to this UUID without a
-- second migration; until that Grafana org exists the column carries the
-- planned target id.
--
-- No user_org rows are seeded here -- /admin/users/link is the canonical
-- path now (control-plane authority, audit logged). Operator runs the
-- bootstrap-token POST after this migration applies.
--
-- plugin_installs rows are NOT seeded either; per-(plugin, org) install
-- is the operator's explicit step via POST /orgs/{org}/plugins/{plugin}.
-- Seeding them would create the LRU + RW schema artefacts without an
-- authenticated caller, which sidesteps the admin-auth model.

INSERT INTO organisations (org_id, short_id, name, grafana_org_id)
VALUES ('00000000-0000-0000-0000-000000000002', 'test2222', 'test2', 2)
ON CONFLICT (org_id) DO NOTHING;
