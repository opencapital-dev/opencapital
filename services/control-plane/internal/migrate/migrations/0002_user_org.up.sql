CREATE TABLE IF NOT EXISTS user_org (
    user_id TEXT NOT NULL,
    org_id  UUID NOT NULL REFERENCES organisations(org_id) ON DELETE CASCADE,
    role    TEXT NOT NULL DEFAULT 'member',
    PRIMARY KEY (user_id, org_id)
);
