-- audit_log is the append-only record of privileged writes against the
-- control plane: every /admin/* call and every /orgs/.../plugins/...
-- install. Both successful and denied attempts produce a row so an
-- operator can see attack patterns alongside legitimate activity.
--
-- result encodes the outcome as a short tag: 'ok', or 'denied:<reason>'
-- where <reason> is one of a closed set (jwt-invalid, self-edit,
-- org-mismatch, last-admin, relink-explicit, bootstrap-disabled,
-- forbidden, rate-limited, ...). No JWT contents or token fragments are
-- ever written to this table.
--
-- actor_source distinguishes which auth path the caller used:
--   'bootstrap'  - ADMIN_BOOTSTRAP_TOKEN bearer
--   'grafana'    - Grafana-issued ID JWT
--   ''           - unauthenticated request (denied before identity was
--                  resolved; actor will be empty too)

CREATE TABLE IF NOT EXISTS audit_log (
    id            BIGSERIAL    PRIMARY KEY,
    at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    actor         TEXT         NOT NULL DEFAULT '',
    actor_source  TEXT         NOT NULL DEFAULT '',
    action        TEXT         NOT NULL,
    target        JSONB        NOT NULL DEFAULT '{}'::jsonb,
    result        TEXT         NOT NULL,
    request_ip    INET
);

CREATE INDEX IF NOT EXISTS audit_log_at_idx     ON audit_log (at DESC);
CREATE INDEX IF NOT EXISTS audit_log_actor_idx  ON audit_log (actor);
CREATE INDEX IF NOT EXISTS audit_log_action_idx ON audit_log (action);
