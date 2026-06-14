-- v6 Phase 3+: seed the dev operator's Kinde identifier in user_org so
-- /jwt/mint resolves their org membership without an out-of-band setup
-- step. Production gets this via the landing/signup app (Phase 7) which
-- inserts user_org rows on Kinde first-login.
--
-- Identifiers come from the Grafana ID JWT's `identifier` claim (Kinde
-- stable sub) AND the `email` claim — control-plane's authenticateGrafana
-- prefers identifier and falls back to email, so seeding both unblocks
-- either path. Adjust the constants below per-developer (they're stable
-- per Kinde user, not per Grafana install).

INSERT INTO user_org (user_id, org_id, role) VALUES
    ('ffn2con6uqayob',                '00000000-0000-0000-0000-000000000001', 'operator'),
    ('iballesterllagaria@gmail.com',  '00000000-0000-0000-0000-000000000001', 'operator')
ON CONFLICT (user_id, org_id) DO NOTHING;
