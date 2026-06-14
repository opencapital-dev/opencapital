#!/usr/bin/env bash
# v6 Phase 6 -- pg_hba entries for the postgres-replica standby.
#
# The postgres docker image lays down a default pg_hba.conf at
# initdb time that only allows password auth from any host for the
# regular roles; it does NOT include a `replication` line. Append two
# entries here (IPv4 + IPv6) so the standby's `replicator` role can
# connect for streaming.
#
# 0.0.0.0/0 is acceptable here because:
#   - The compose `platform` network is internal-only on prod (no host
#     ports published beyond Caddy), so non-stack hosts can't reach
#     5432 at all.
#   - `replicator` is a dedicated REPLICATION-only role with no other
#     grants; even if its password leaked, the attacker gets a WAL
#     stream from a database they already have no other path to read.
#
# Reload signal goes through SELECT pg_reload_conf() so we don't
# bounce the server between init-script invocations.

set -euo pipefail

cat >> "${PGDATA}/pg_hba.conf" <<'EOF'

# v6 Phase 6 streaming replication (added by init script).
host    replication     replicator      0.0.0.0/0       md5
host    replication     replicator      ::/0            md5
EOF

psql -v ON_ERROR_STOP=1 --username "${POSTGRES_USER}" --dbname "${POSTGRES_DB}" \
    -c "SELECT pg_reload_conf();"
