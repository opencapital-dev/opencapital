-- v6 Phase 3: plugin_installs gains a per-(org, plugin) platform_token. The
-- token is the plugin-side authenticator at POST /jwt/mint and the seed for
-- per-(plugin, org) SQLite encryption keys (HKDF-derived). In production the
-- install endpoint generates one per install; this migration backfills
-- deterministic dev tokens for the seeded rows so docker-compose can wire
-- them into plugin containers without out-of-band coordination.

ALTER TABLE plugin_installs ADD COLUMN platform_token TEXT;

UPDATE plugin_installs
   SET platform_token = 'dev-' || plugin_id || '-platform-token-DO-NOT-USE-IN-PROD'
 WHERE platform_token IS NULL;

ALTER TABLE plugin_installs ALTER COLUMN platform_token SET NOT NULL;
