#!/usr/bin/env bash
# In-distro data-plane supervisor for the OpenCapital WSL distro (Windows).
#
# Brings up the headless data plane INSIDE the WSL2 distro, in the same order
# the macOS native supervisor uses (opencapital-app/src-tauri/src/dataplane.rs
# bring_up): postgres -> control_db bootstrap -> control-plane -> RisingWave ->
# apply RW schema -> gateway -> read-gateway. Then blocks on `wait` so the
# distro stays alive (WSL terminates a distro once its last process exits) and
# the host can health-check each service over `localhost` (WSL2 NAT forwards
# host -> distro for 0.0.0.0-bound listeners).
#
# Identity/registry config arrives as env from the host (WSLENV-forwarded):
#   KINDE_DOMAIN, KINDE_AUDIENCE  (required — control-plane validates the
#                                  shell's Kinde tokens against the same tenant)
#   REGISTRY_OWNER                (optional, default opencapital-dev)
#   REGISTRY_PASSWORD             (optional, dev-only GHCR enumeration token)
#
# Paths are baked into the rootfs by infra/wsl/Dockerfile.rootfs. State lives
# under /data (inside the distro's ext4) so it survives in-place binary updates.
set -euo pipefail

PREFIX=/opt/opencapital
DATA=/data
PGBIN=/usr/lib/postgresql/17/bin
LOG="$PREFIX/logs"
export PATH="$PGBIN:$PREFIX/bin:$PATH"

mkdir -p "$DATA/pgdata" "$DATA/rw-store" "$LOG"

# Local dev creds — match infra/postgres/init/01-schema.sql + dataplane.rs.
CONTROL_DB_DSN="postgres://control_plane:control_plane_pw@127.0.0.1:5432/control_db?sslmode=disable"
GATEWAY_REPLICA_DSN="postgres://postgres@127.0.0.1:5432/control_db?sslmode=disable"
RW_DSN="postgres://root@127.0.0.1:4566/dev?sslmode=disable"
CONTROL_PLANE_URL="http://127.0.0.1:18080"
LOCAL_TOKEN="localbootstrap"

KINDE_DOMAIN="${KINDE_DOMAIN%/}"
: "${KINDE_AUDIENCE:?KINDE_AUDIENCE required}"
: "${KINDE_DOMAIN:?KINDE_DOMAIN required}"
KINDE_JWKS_URL="${KINDE_DOMAIN}/.well-known/jwks.json"
REGISTRY_OWNER="${REGISTRY_OWNER:-opencapital-dev}"

# wait_tcp <host> <port> [timeout_s] — block until a port accepts (bash /dev/tcp,
# no nc dependency). RW + a fresh pg cluster both take a while on first run.
wait_tcp() {
  local host="$1" port="$2" deadline=$(( SECONDS + ${3:-120} ))
  until (exec 3<>"/dev/tcp/${host}/${port}") 2>/dev/null; do
    exec 3>&- 2>/dev/null || true
    [ "$SECONDS" -ge "$deadline" ] && { echo "timeout waiting for ${host}:${port}" >&2; return 1; }
    sleep 0.5
  done
  exec 3>&- 2>/dev/null || true
}

# 1. postgres (wal_level=logical for RW's postgres-cdc source).
if [ ! -f "$DATA/pgdata/PG_VERSION" ]; then
  echo "initdb…" >&2
  initdb -D "$DATA/pgdata" -U postgres --auth=trust --encoding=UTF8 >"$LOG/pg-initdb.log" 2>&1
fi
postgres -D "$DATA/pgdata" -p 5432 \
  -c listen_addresses=127.0.0.1 \
  -c wal_level=logical -c max_replication_slots=8 -c max_wal_senders=8 \
  >"$LOG/postgres.log" 2>&1 &
wait_tcp 127.0.0.1 5432

# 2. control_db bootstrap (db + roles), once. control-plane auto-migrates it.
if [ "$(psql -h 127.0.0.1 -U postgres -d postgres -tAc \
      "SELECT 1 FROM pg_database WHERE datname='control_db'")" != "1" ]; then
  echo "bootstrap control_db…" >&2
  psql -h 127.0.0.1 -U postgres -d postgres -c "CREATE DATABASE control_db;"
  psql -h 127.0.0.1 -U postgres -d control_db -v ON_ERROR_STOP=1 \
    -f "$PREFIX/infra/postgres/init/01-schema.sql"
fi

# 3. control-plane — migrates control_db (portfolios + rw_v6_pub publication).
CONTROL_DB_DSN="$CONTROL_DB_DSN" \
LISTEN_ADDR=":18080" \
IDP_STATIC_USERS='[{"user_id":"admin","token":"localbootstrap"}]' \
ADMIN_BOOTSTRAP_TOKEN="$LOCAL_TOKEN" \
CONTROL_PLANE_JWKS_URL="http://127.0.0.1:18080/jwt/jwks" \
KINDE_JWKS_URL="$KINDE_JWKS_URL" \
KINDE_ISSUER="$KINDE_DOMAIN" \
KINDE_AUDIENCE="$KINDE_AUDIENCE" \
RISINGWAVE_DSN="$RW_DSN" \
REGISTRY_INTERNAL_URL="https://ghcr.io" \
REGISTRY_PUBLIC_URL="https://ghcr.io" \
REGISTRY_NAMESPACE="plugins" \
REGISTRY_STAGING_NAMESPACE="plugins-staging" \
REGISTRY_OWNER="$REGISTRY_OWNER" \
PLUGINS_MANIFEST_URL="https://raw.githubusercontent.com/opencapital-dev/opencapital-releases/main/plugins.json" \
  "$PREFIX/bin/control-plane" >"$LOG/control-plane.log" 2>&1 &
wait_tcp 127.0.0.1 18080

# 4. RisingWave single-node (embedded Python UDF for fold_kernel). The Linux
#    binary ships in the official image with its connector libs + embedded
#    python already wired — no PYTHONHOME/CONNECTOR_LIBS_PATH relinking needed
#    (unlike the relinked macOS bottle).
RW_BIN="${RW_BIN:-/risingwave/bin/risingwave}"
RW_SINGLE_NODE_CONFIG_PATH="$PREFIX/risingwave/config.toml" \
RW_SINGLE_NODE_STORE_DIRECTORY="$DATA/rw-store" \
  "$RW_BIN" single-node >"$LOG/risingwave.log" 2>&1 &
wait_tcp 127.0.0.1 4566

# 5. Apply local RW schema (connector-less tables + MVs + pg CDC source).
#    Idempotent (apply.sh tracks _schema_migrations). apply.sh resolves its
#    schema dirs relative to its own directory, so run it from there.
( cd "$PREFIX/infra/risingwave" && \
  PACKAGING=local CDC_PG_HOST=127.0.0.1 \
  RW_HOST=localhost RW_PORT=4566 RW_USER=root RW_DB=dev \
  UDF_HOST=localhost UDF_PORT=4566 \
    bash apply.sh ) >"$LOG/rw-apply.log" 2>&1

# 6. gateway (SINK_MODE=rw — pgwire DML into RW).
LISTEN_ADDR=":8090" \
SINK_MODE="rw" \
RW_DSN="$RW_DSN" \
CONTROL_PLANE_URL="$CONTROL_PLANE_URL" \
CONTROL_DB_REPLICA_DSN="$GATEWAY_REPLICA_DSN" \
LRU_PRIME_TOKEN="$LOCAL_TOKEN" \
  "$PREFIX/bin/gateway" >"$LOG/gateway.log" 2>&1 &
wait_tcp 127.0.0.1 8090

# 7. read-gateway (sole RW reader).
LISTEN_ADDR=":8095" \
CONTROL_PLANE_URL="$CONTROL_PLANE_URL" \
RISINGWAVE_DSN="$RW_DSN" \
  "$PREFIX/bin/read-gateway" >"$LOG/read-gateway.log" 2>&1 &
wait_tcp 127.0.0.1 8095

echo "data plane up" >&2
wait
