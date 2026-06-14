CREATE TABLE IF NOT EXISTS jwt_signing_keys (
    kid         TEXT PRIMARY KEY,
    alg         TEXT NOT NULL,
    private_pem TEXT NOT NULL,
    public_pem  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retires_at  TIMESTAMPTZ
);
