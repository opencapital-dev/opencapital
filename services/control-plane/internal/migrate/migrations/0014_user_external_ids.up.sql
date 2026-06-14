-- user_external_ids maps a control-plane canonical user_id (the value
-- stored in user_org.user_id) to one or more provider-specific external
-- handles that may appear in upstream JWTs (e.g. Grafana's "user:N" sub
-- claim, Kinde's opaque sub claim).
--
-- PK is (provider, external_id) so each external handle resolves to
-- exactly one canonical user_id. A canonical user_id may have multiple
-- external mappings (e.g. linked to both a Grafana user AND a Kinde
-- account), which is why user_id is not part of the PK.
--
-- created_by carries the actor that produced the row: a control-plane
-- user_id when an admin called /admin/users/link via the Grafana JWT
-- path, or the literal 'bootstrap' when the call was made via the
-- env-gated ADMIN_BOOTSTRAP_TOKEN.

CREATE TABLE IF NOT EXISTS user_external_ids (
    provider    TEXT        NOT NULL,
    external_id TEXT        NOT NULL,
    user_id     TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by  TEXT        NOT NULL,
    PRIMARY KEY (provider, external_id)
);

CREATE INDEX IF NOT EXISTS user_external_ids_user_id_idx
    ON user_external_ids (user_id);
