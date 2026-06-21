#!/usr/bin/env bash
# Apply the RisingWave schema.
#
# Squashed baseline (one-shot). Applies the complete, org_id-free schema in
# dependency order under dataplane/risingwave/schemas/ and records the marker
# `V001__v2_baseline` in `_schema_migrations` with plugin_id='core'. This is
# the single source of truth. Historical core migrations (V002-V007) and the
# centralized plugin-migration mechanism have been retired. Plugins now ship
# their own RW migrations via the SDK rwmigrate framework.
#
#   00-bootstrap.sql      → _schema_migrations tracker (with plugin_id)
#   01-sources/           → portfolio_events.v2, data.v2 Kafka tables
#   02-control-plane/     → pg_cdc SOURCE + portfolios/instruments CDC tables
#                            (sourced from the Postgres OLTP store; see
#                             infra/postgres/init/), plus the native
#                             `checkpoints` table.
#                            (yfinance state is in-memory; see
#                            services/ingestor-yfinance/backfill_queue.py)
#   03-functions/         → fold_kernel UDAF (waits for udf-server first)
#   03-unifying-views/    → option_marks MV, prices MV
#   03b-instruments/      → instruments MV
#   04-fx/                → fx_rates MV
#   04b-events/           → events MV
#   05-fold/              → fold_per_event MV
#   06-metrics/           → per-tick/per-event metric MVs
#   07-snapshots/         → latest_portfolio_state VIEW
#   08-ingestor-discovery/→ instruments_used MV, fx_pairs_used MV,
#                            ohlcv_coverage VIEW, data_coverage VIEW,
#                            instruments_catalog VIEW
#   10-entities/          → e_portfolio, e_nav, e_instrument, e_cash,
#                            e_price, e_events, e_flows, e_closures,
#                            e_cycles VIEWs
#
# Usage:
#   ./apply.sh                       # connect to localhost:4566 from host
#   RW_HOST=risingwave ./apply.sh    # connect to risingwave:4566 from within compose

set -euo pipefail

RW_HOST="${RW_HOST:-localhost}"
RW_PORT="${RW_PORT:-4566}"
RW_USER="${RW_USER:-root}"
RW_DB="${RW_DB:-dev}"

HERE="$(cd "$(dirname "$0")" && pwd)"
V2_SCHEMA_DIR="$HERE/schemas"

# psql client selection — host psql first, fall back to a one-shot
# postgres:17 client container on the compose network (Postgres image's
# psql ships with libpq client only; no server needed).
NETWORK_NAME="portfolio-management-v2_platform"
USE_STDIN=0
if command -v psql >/dev/null 2>&1; then
    PSQL=(psql -h "$RW_HOST" -p "$RW_PORT" -U "$RW_USER" -d "$RW_DB"
          -v ON_ERROR_STOP=1 --no-psqlrc)
else
    echo "psql not on PATH; using one-shot postgres:17 client container..."
    PSQL=(docker run --rm -i --network "$NETWORK_NAME" postgres:17
          psql -h risingwave -p 4566 -U root -d dev
          -v ON_ERROR_STOP=1 --no-psqlrc)
    USE_STDIN=1
fi

echo "waiting for RisingWave at $RW_HOST:$RW_PORT ..."
for i in $(seq 1 240); do
    if "${PSQL[@]}" -c 'SELECT 1' >/dev/null 2>&1; then
        echo "  ready"
        break
    fi
    sleep 0.5
done

run_sql() {
    "${PSQL[@]}" -c "$1"
}

# Secrets to substitute into source SQL at apply time. The
# @@VAR_NAME@@ placeholders in committed SQL get replaced with the
# literal values read from the named compose-secret file (or env
# var). Used by the Kafka source SQL to inject SR Basic Auth
# passwords without committing them.
# SECRETS_DIR points at the materialised SOPS output (`make
# secrets-sync` writes infra/secrets/<name>). Default lets `make
# rw-apply` work standalone after `make secrets-sync`.
: "${SECRETS_DIR:=$HERE/../secrets}"

# macOS /bin/bash is 3.2 (no associative arrays). The downloaded .app launches
# from Finder with a minimal PATH, so this script runs under /bin/bash 3.2 — it
# must avoid bash 4+ features (assoc arrays, mapfile). Parallel indexed arrays
# keyed by position stand in for the placeholder->value map.
SQL_SUB_KEYS=()
SQL_SUB_VALS=()
load_substitution() {
    local placeholder="$1" file_or_env="$2"
    local value=""
    if [[ -n "${!file_or_env:-}" ]]; then
        value="${!file_or_env}"
    elif [[ -n "${SECRETS_DIR:-}" && -f "${SECRETS_DIR}/${file_or_env}" ]]; then
        value="$(cat "${SECRETS_DIR}/${file_or_env}")"
    fi
    if [[ -n "${value}" ]]; then
        SQL_SUB_KEYS+=("${placeholder}")
        SQL_SUB_VALS+=("${value}")
    fi
}
# CDC source postgres host: compose/nomad DNS 'postgres' by default; the local
# desktop packaging overrides via CDC_PG_HOST=127.0.0.1. Always substituted so
# the @@CDC_PG_HOST@@ placeholder never reaches RW literally.
SQL_SUB_KEYS+=("@@CDC_PG_HOST@@")
SQL_SUB_VALS+=("${CDC_PG_HOST:-postgres}")

run_file() {
    local file="$1"
    echo "==> applying $file"
    if [[ ${#SQL_SUB_KEYS[@]} -gt 0 ]]; then
        # Stream the file through sed substitutions before piping to
        # psql. Substitutions only apply to files that contain a
        # placeholder; the rest stream through unchanged.
        local sed_args=()
        local i k v
        for i in "${!SQL_SUB_KEYS[@]}"; do
            k="${SQL_SUB_KEYS[$i]}"
            # Escape characters that have meaning in sed's s///
            v="${SQL_SUB_VALS[$i]}"
            v="${v//\\/\\\\}"
            v="${v//&/\\&}"
            v="${v//\//\\/}"
            sed_args+=( -e "s/${k}/${v}/g" )
        done
        sed "${sed_args[@]}" "$file" | "${PSQL[@]}"
    elif [[ "$USE_STDIN" == "1" ]]; then
        cat "$file" | "${PSQL[@]}"
    else
        "${PSQL[@]}" -f "$file"
    fi
}

is_applied() {
    local plugin="$1" version="$2"
    local out
    out="$("${PSQL[@]}" -tA -c "SELECT 1 FROM _schema_migrations WHERE plugin_id = '$plugin' AND version = '$version';" 2>/dev/null || true)"
    [[ "$out" == "1" ]]
}

record() {
    local plugin="$1" version="$2" name="$3"
    run_sql "INSERT INTO _schema_migrations (plugin_id, version, name) VALUES ('$plugin', '$version', '$name');"
}

wait_for_udf() {
    local host="${UDF_HOST:-localhost}"
    local port="${UDF_PORT:-8815}"
    echo "==> waiting for UDF server at $host:$port ..."
    for i in $(seq 1 240); do
        if (echo > /dev/tcp/"$host"/"$port") >/dev/null 2>&1; then
            echo "  ready"
            return 0
        fi
        sleep 0.5
    done
    echo "  UDF server not reachable; CREATE FUNCTION will fail" >&2
    return 1
}

# Session settings — BACKGROUND_DDL lets MVs backfill from Kafka `earliest`
# without blocking.
echo "==> session settings (BACKGROUND_DDL)"
run_sql "SET BACKGROUND_DDL = true;"

# ---- _schema_migrations bootstrap (must precede everything) ---------------
# Defined in infra/risingwave/schemas/00-bootstrap.sql; replicated here so
# apply.sh has a tracker to consult before any SQL file ever runs. Idempotent.
echo "==> ensuring _schema_migrations tracker (with plugin_id)"
run_sql "CREATE TABLE IF NOT EXISTS _schema_migrations (
    plugin_id   VARCHAR NOT NULL DEFAULT 'core',
    version     VARCHAR NOT NULL,
    name        VARCHAR NOT NULL,
    applied_at  TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (plugin_id, version)
);"

# ---- Phase A: v2 baseline --------------------------------------------------
if is_applied "core" "V001__v2_baseline"; then
    echo "==> v2 baseline already applied, skipping"
else
    echo "==> v2 baseline: applying schemas under $V2_SCHEMA_DIR"
    # Walk every numbered subdirectory in path-sort order. Files in
    # */functions/ trigger wait_for_udf before the apply; otherwise just
    # apply.
    v2_files=()
    while IFS= read -r line; do v2_files+=("$line"); done < <(find "$V2_SCHEMA_DIR" -name '*.sql' -type f | LC_ALL=C sort)
    udf_ready=0
    for path in "${v2_files[@]}"; do
        rel="${path#"$V2_SCHEMA_DIR/"}"
        if [[ "$rel" == *"/functions/"* ]] && [[ "$udf_ready" == "0" ]]; then
            wait_for_udf
            udf_ready=1
        fi
        run_file "$path"
    done
    record "core" "V001__v2_baseline" "v2_baseline"
fi

echo "==> DDL progress (any rows = MV still backfilling)"
run_sql "SELECT ddl_id, ddl_statement, progress FROM rw_catalog.rw_ddl_progress;"

echo "==> migration state"
run_sql "SELECT plugin_id, version, name, applied_at FROM _schema_migrations ORDER BY plugin_id, version;"

echo "==> objects landed"
run_sql "SELECT name, connector FROM rw_catalog.rw_sources ORDER BY name;
         SELECT name FROM rw_catalog.rw_tables WHERE schema_id = (SELECT id FROM rw_catalog.rw_schemas WHERE name='public') ORDER BY name;
         SELECT name FROM rw_catalog.rw_materialized_views WHERE schema_id = (SELECT id FROM rw_catalog.rw_schemas WHERE name='public') ORDER BY name;"

echo "RisingWave schema applied."
