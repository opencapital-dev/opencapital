#!/usr/bin/env python3
"""Phase 1 parity check — RisingWave obs_* / price_* vs QuestDB obs_*.

The Phase 1 exit criterion in docs/risingwave/06-migration-plan.md says:

    RisingWave's `events` / `prices` tables match QuestDB's `obs_*` row counts
    and content over a sustained window.

This script does both halves of that comparison:

1. **Per-day count diff** — `GROUP BY (scope_id, day(event_ts))` on both
   sides; small ingestion-lag differences are noise (Kafka commit cadence,
   QuestDB ILP flush) — anything sustained is a real divergence.
2. **Per-entity-id existence diff** — for tables with a stable id
   (`trade_id`, `dividend_id`, `cashflow_id`, `fx_conversion_id`, `lot_id`):
   which ids exist only on one side. RisingWave-only ids that arrived after
   QuestDB's last sync are normal lag; QuestDB-only ids that *persist* are a
   real gap. RisingWave-tombstone-deleted ids that QuestDB still holds are
   also reported — expected today (QuestDB sinks don't model deletes) but
   the report makes the divergence visible.

Run as one-shot from `make rw-parity`. Exits 0 when all comparisons pass the
soft tolerance, 1 otherwise. Soft tolerance is intentionally lenient — this
is a soak check, not a unit test. Tighten before the Phase 1 exit gate.

Env (with sensible localhost defaults for `make rw-parity`):

    RW_DSN        postgresql://root@localhost:4566/dev
    QUESTDB_DSN   postgresql://admin:quest@localhost:8812/qdb
    DAYS          how many recent days to compare (default 7)
    MAX_DRIFT     allowed per-(scope, day) row-count delta (default 5)
"""
from __future__ import annotations

import os
import sys
from dataclasses import dataclass

import psycopg


@dataclass(frozen=True)
class TablePair:
    rw_table: str         # RisingWave table name
    qdb_table: str        # QuestDB table name
    id_col: str | None    # primary-id column (None for price tables — no stable id)


# Legacy v1 mapping: RW v1 obs_* tables vs the now-decommissioned QuestDB
# obs_*s tables. Both schemas have been removed (QuestDB in Phase 4 of the
# Postgres-decom plan; v1 RW tables in the v2 cutover). This script is
# kept as a historical reference; see docs/risingwave/v2/06-cutover-report.md
# for v2 verification mechanics. Names diverge in pluralization (QuestDB
# carries the older plural names; RW uses the singular Avro envelope names)
# and in tier (price_quote / price_ohlcv_bar on RW vs obs_quotes /
# obs_ohlcv_bars on QuestDB — RW reorganizes prices into their own
# append-only tier per ADR-0005).
TABLES: tuple[TablePair, ...] = (
    TablePair("obs_trade",         "obs_trades",         "trade_id"),
    TablePair("obs_dividend",      "obs_dividends",      "dividend_id"),
    TablePair("obs_cashflow",      "obs_cashflows",      "cashflow_id"),
    TablePair("obs_fx_conversion", "obs_fx_conversions", "fx_conversion_id"),
    TablePair("obs_transfer_in",   "obs_transfer_ins",   "lot_id"),
    TablePair("price_quote",       "obs_quotes",         None),
    TablePair("price_ohlcv_bar",   "obs_ohlcv_bars",     None),
)


def _env_dsn(key: str, default: str) -> str:
    return os.environ.get(key) or default


def daily_counts(conn: psycopg.Connection, table: str, days: int, dialect: str) -> dict[tuple[str, str], int]:
    # Day buckets as `YYYY-MM-DD` strings so RW and QDB results compare cleanly
    # regardless of underlying TIMESTAMP/DATE types or timezones.
    if dialect == "qdb":
        # QuestDB: dateadd for the cutoff; to_str for the day key.
        sql = f"""
            SELECT scope_id, to_str(date_trunc('day', event_ts), 'yyyy-MM-dd') AS d, COUNT(*)
            FROM {table}
            WHERE event_ts >= dateadd('d', -{days}, now())
            GROUP BY scope_id, d
        """
    else:
        sql = f"""
            SELECT scope_id, to_char(date_trunc('day', event_ts), 'YYYY-MM-DD') AS d, COUNT(*)
            FROM {table}
            WHERE event_ts >= NOW() - INTERVAL '{days} day'
            GROUP BY scope_id, d
        """
    with conn.cursor() as cur:
        cur.execute(sql)
        return {(scope, d): int(n) for scope, d, n in cur.fetchall()}


def ids_present(conn: psycopg.Connection, table: str, id_col: str, days: int, dialect: str) -> set[str]:
    if dialect == "qdb":
        sql = f"SELECT {id_col} FROM {table} WHERE event_ts >= dateadd('d', -{days}, now())"
    else:
        sql = f"SELECT {id_col} FROM {table} WHERE event_ts >= NOW() - INTERVAL '{days} day'"
    with conn.cursor() as cur:
        cur.execute(sql)
        return {str(row[0]) for row in cur.fetchall() if row[0] is not None}


def compare_counts(
    rw: dict[tuple[str, str], int],
    qdb: dict[tuple[str, str], int],
    max_drift: int,
) -> tuple[int, list[str]]:
    keys = set(rw) | set(qdb)
    bad: list[str] = []
    drift_total = 0
    for k in sorted(keys):
        r = rw.get(k, 0)
        q = qdb.get(k, 0)
        delta = abs(r - q)
        drift_total += delta
        if delta > max_drift:
            scope, day = k
            bad.append(f"  ({scope}, {day}): RW={r} QDB={q} drift={delta}")
    return drift_total, bad


def compare_ids(rw_ids: set[str], qdb_ids: set[str]) -> tuple[set[str], set[str]]:
    return rw_ids - qdb_ids, qdb_ids - rw_ids


def main() -> int:
    rw_dsn = _env_dsn("RW_DSN", "postgresql://root@localhost:4566/dev")
    qdb_dsn = _env_dsn("QUESTDB_DSN", "postgresql://admin:quest@localhost:8812/qdb")
    days = int(os.environ.get("DAYS", "7"))
    max_drift = int(os.environ.get("MAX_DRIFT", "5"))

    print("== Phase 1 parity check ==")
    print(f"  RW   : {rw_dsn}")
    print(f"  QDB  : {qdb_dsn}")
    print(f"  days : last {days}")
    print(f"  drift: tolerated <= {max_drift} rows per (scope, day)")

    failures = 0
    summary: list[str] = []

    with psycopg.connect(rw_dsn, autocommit=True) as rw_conn, \
         psycopg.connect(qdb_dsn, autocommit=True) as qdb_conn:
        for pair in TABLES:
            print(f"\n-- {pair.rw_table} (RW) vs {pair.qdb_table} (QDB) --")
            try:
                rw_counts = daily_counts(rw_conn, pair.rw_table, days, "rw")
            except Exception as exc:
                print(f"  RW query failed: {exc}")
                failures += 1
                summary.append(f"{pair.rw_table}: RW query failed")
                continue
            try:
                qdb_counts = daily_counts(qdb_conn, pair.qdb_table, days, "qdb")
            except Exception as exc:
                print(f"  QDB query failed: {exc}")
                failures += 1
                summary.append(f"{pair.rw_table}: QDB query failed")
                continue

            rw_total = sum(rw_counts.values())
            qdb_total = sum(qdb_counts.values())
            drift_total, bad = compare_counts(rw_counts, qdb_counts, max_drift)
            print(f"  totals: RW={rw_total} QDB={qdb_total} drift_total={drift_total}")
            if bad:
                print(f"  per-(scope, day) buckets exceeding drift={max_drift}:")
                for line in bad:
                    print(line)
                failures += len(bad)
                summary.append(f"{pair.rw_table}: {len(bad)} bucket(s) over drift")

            if pair.id_col is not None:
                try:
                    rw_ids = ids_present(rw_conn, pair.rw_table, pair.id_col, days, "rw")
                    qdb_ids = ids_present(qdb_conn, pair.qdb_table, pair.id_col, days, "qdb")
                except Exception as exc:
                    print(f"  id-set query failed: {exc}")
                    continue
                rw_only, qdb_only = compare_ids(rw_ids, qdb_ids)
                if rw_only or qdb_only:
                    print(f"  ids only on RW : {len(rw_only)}  (sample: {sorted(rw_only)[:3]})")
                    print(f"  ids only on QDB: {len(qdb_only)}  (sample: {sorted(qdb_only)[:3]})")
                # RW-only ids are usually ingestion lag (RW ahead of the
                # sink); QDB-only ids are tombstone-deleted rows on RW
                # (expected — sinks don't model deletes today, see Phase 1
                # plan step 6).

    print("\n== summary ==")
    if summary:
        for line in summary:
            print(f"  FAIL  {line}")
    else:
        print("  all tables within tolerance.")
    return 0 if failures == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
