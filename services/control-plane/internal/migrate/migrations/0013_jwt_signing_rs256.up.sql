-- v6 Phase 3: control-plane signing switches from ES256 to RS256.
-- RisingWave v2.8.x's OAuth user binding parses JWKs assuming RSA shape
-- (`n`, `e`); ES256 / EC keys are rejected with "missing field `n`".
-- The gateway accepts both via its JWKS client.
--
-- Delete the existing ES256 row so EnsureBootstrap generates a fresh
-- RS256 keypair on next control-plane boot. Old session JWTs signed by
-- the deleted key become unverifiable; clients re-mint on first 401.

DELETE FROM jwt_signing_keys WHERE alg = 'ES256';
