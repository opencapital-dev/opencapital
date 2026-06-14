-- plugin_installs is a grant (entitlement + platform_token), not an inventory.
-- The org carries no version; version selection is a local desktop concern.
ALTER TABLE plugin_installs DROP COLUMN version;
ALTER TABLE plugin_installs RENAME COLUMN installed_at TO granted_at;
