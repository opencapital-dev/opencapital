#!/bin/bash
# In-distro data-plane supervisor for the OpenCapital WSL distro (Windows).
#
# Brings up the headless data plane INSIDE the WSL2 distro, mirroring the
# macOS native boot sequence in opencapital-app/src-tauri/src/dataplane.rs:
#   postgres -> control_db bootstrap (01-schema.sql + 02-portfolios.sql) ->
#   RisingWave -> apply RW schema (Phase A: connector-less tables + MVs +
#   pg CDC source).
#
# Then blocks on `wait` so the distro stays alive (WSL terminates a distro
# once its last process exits); the host health-checks each service over
# `localhost` (WSL2 NAT forwards host -> distro for 0.0.0.0-bound listeners).
#
# Identity/registry config (REGISTRY_OWNER, REGISTRY_PASSWORD) arrives via
# WSLENV-forwarded env from the host when needed.
#
# Paths are baked into the rootfs by dataplane/wsl/Dockerfile.rootfs. State
# lives under /data (inside the distro's ext4) so it survives in-place binary
# updates.
set -euo pipefail

PREFIX=/opt/opencapital
DATA=/data
PGBIN=/usr/lib/postgresql/17/bin
LOG="$PREFIX/logs"
export PATH="$PGBIN:$PREFIX/bin:$PATH"

mkdir -p "$DATA/pgdata" "$DATA/rw-store" "$LOG"

RW_DSN="postgres://root@127.0.0.1:4566/dev?sslmode=disable"

REGISTRY_OWNER="${REGISTRY_OWNER:-opencapital-dev}"

# wait_tcp <host> <port> [timeout_s] — block until a port accepts (bash /dev/tcp,
# no nc dependency). RW + a fresh pg cluster both take a while on first run.
wait_tcp() {
  local host="$1" port="$2" deadline
  deadline=$(( SECONDS + ${3:-120} ))
  until (exec 3<>"/dev/tcp/${host}/${port}") 2>/dev/null; do
    exec 3>&- 2>/dev/null || true
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "timeout waiting for ${host}:${port}" >&2
      return 1
    fi
    sleep 1
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

# Wait until postgres is actually answering queries (not just accepting TCP
# connections — it opens the listener before finishing crash recovery).
deadline=$(( SECONDS + 120 ))
until psql -h 127.0.0.1 -U postgres -d postgres -tAc "SELECT 1" >/dev/null 2>&1; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "postgres not query-ready within 120s" >&2
    exit 1
  fi
  sleep 1
done

# 2. Bootstrap control_db (db + roles), once (idempotent guard on pg_database).
if [ "$(psql -h 127.0.0.1 -U postgres -d postgres -tAc \
      "SELECT 1 FROM pg_database WHERE datname='control_db'")" != "1" ]; then
  echo "bootstrap control_db…" >&2
  psql -h 127.0.0.1 -U postgres -d postgres -c "CREATE DATABASE control_db;"
  psql -h 127.0.0.1 -U postgres -d control_db -v ON_ERROR_STOP=1 \
    -f "$PREFIX/infra/postgres/init/01-schema.sql"
fi

# 3. Apply portfolios DDL + rw_v6_pub publication (idempotent: CREATE TABLE IF
#    NOT EXISTS / IF NOT EXISTS guards). Mirrors dataplane.rs bootstrap_control_db
#    which runs 02-portfolios.sql on every boot.
psql -h 127.0.0.1 -U postgres -d control_db -v ON_ERROR_STOP=1 \
  -f "$PREFIX/infra/postgres/init/02-portfolios.sql" \
  >"$LOG/portfolios-ddl.log" 2>&1

# 4. RisingWave single-node (embedded Python UDF for fold_kernel). The Linux
#    binary ships in the official image with its connector libs + embedded
#    python already wired — no PYTHONHOME/CONNECTOR_LIBS_PATH relinking needed
#    (unlike the relinked macOS bottle).
RW_BIN="${RW_BIN:-/risingwave/bin/risingwave}"
RW_SINGLE_NODE_CONFIG_PATH="$PREFIX/risingwave/config.toml" \
RW_SINGLE_NODE_STORE_DIRECTORY="$DATA/rw-store" \
  "$RW_BIN" single-node >"$LOG/risingwave.log" 2>&1 &
wait_tcp 127.0.0.1 4566

# 5. Apply local RW schema (Phase A: connector-less tables + MVs + pg CDC
#    source). Idempotent (apply.sh tracks _schema_migrations). apply.sh resolves
#    its schema dirs relative to its own directory, so run it from there.
( cd "$PREFIX/infra/risingwave" && \
  PACKAGING=local CDC_PG_HOST=127.0.0.1 \
  RW_HOST=localhost RW_PORT=4566 RW_USER=root RW_DB=dev \
  UDF_HOST=localhost UDF_PORT=4566 \
    bash apply.sh ) >"$LOG/rw-apply.log" 2>&1

echo "data plane up" >&2
wait
