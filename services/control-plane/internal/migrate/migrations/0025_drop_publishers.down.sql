CREATE TABLE publishers (
    id                     BIGSERIAL PRIMARY KEY,
    oidc_identity_pattern  TEXT NOT NULL,
    can_push               BOOLEAN NOT NULL DEFAULT TRUE,
    auto_promote           BOOLEAN NOT NULL DEFAULT FALSE,
    note                   TEXT NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Local dev: the Dex signer identity (issuer|email). Any artifact signed by
-- the local Dex stack auto-promotes, so `make release` flows through the real
-- gate instantly.
INSERT INTO publishers (oidc_identity_pattern, can_push, auto_promote, note) VALUES
  ('http://dex:5556/dex|*', true, true, 'local dev Dex signer'),
  ('https://token.actions.githubusercontent.com|REPLACE_WITH_GH_ORG/*', true, false, 'first-party plugin repos placeholder — set the real GitHub org/repo AND flip auto_promote=true once trusted');
