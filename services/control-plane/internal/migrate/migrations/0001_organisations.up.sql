CREATE TABLE IF NOT EXISTS organisations (
    org_id     UUID PRIMARY KEY,
    short_id   TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
