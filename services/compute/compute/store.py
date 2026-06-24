"""Dual-store query layer: one pg8000 connection to RisingWave, one to
Postgres control_db. Both speak pgwire, so rwclient's query/_frame_from serve
both. A QuerySpec carries store="auto"|"rw"|"pg"; auto is resolved by router.

Connections are opened lazily (on first catalog/run call) so that the store
can be constructed without a live database — the first actual query raises
if the DSN is unreachable.  This matters for auto-routed bindings: the
``tables_in(spec.sql)`` parse (sqlglot) is evaluated BEFORE ``self.catalog()``
opens the connection, so the freeze smoke test can prove the postgres dialect
loads in the frozen binary even when no database is available.

If Postgres is configured but unreachable, ``catalog()`` logs a warning and
returns an RW-only catalog so that auto-routed RW panels keep working.
Explicit ``pg()`` bindings will then fail at ``run()`` (no pg conn available).
"""
from __future__ import annotations
import logging
import polars as pl
from compute import rwclient
from compute.contract import QuerySpec
from compute.router import tables_in, decide_store

_log = logging.getLogger(__name__)

def rw(sql: str, *params) -> QuerySpec:
    return QuerySpec("rw", sql, tuple(params))

def pg(sql: str, *params) -> QuerySpec:
    return QuerySpec("pg", sql, tuple(params))

_RW_CATALOG_SQL = (
    "SELECT name FROM rw_catalog.rw_tables "
    "UNION ALL SELECT name FROM rw_catalog.rw_materialized_views "
    "UNION ALL SELECT name FROM rw_catalog.rw_views"
)
_PG_CATALOG_SQL = (
    "SELECT table_name AS name FROM information_schema.tables "
    "WHERE table_schema NOT IN ('pg_catalog','information_schema')"
)

class Store:
    def __init__(self, rw_dsn: str, pg_dsn: str | None):
        # Store DSNs; connections are opened lazily on first use so construction
        # never raises — the error surfaces on the first actual query instead.
        self._rw_dsn = rw_dsn
        self._pg_dsn = pg_dsn
        self._rw = None
        self._pg = None
        self._catalog: dict[str, str] | None = None

    def _conn_rw(self):
        if self._rw is None:
            self._rw = rwclient.connect(self._rw_dsn)
        return self._rw

    def _conn_pg(self):
        if self._pg is None and self._pg_dsn is not None:
            self._pg = rwclient.connect(self._pg_dsn)
        return self._pg

    def catalog(self) -> dict[str, str]:
        if self._catalog is None:
            # Build RW catalog first — this must succeed for any query routing.
            cat: dict[str, str] = {}
            for name in rwclient.query(self._conn_rw(), _RW_CATALOG_SQL)["name"].to_list():
                cat[name] = "rw"
            # PG catalog is best-effort: a down/unreachable Postgres must not
            # break RW-only queries.  Explicit pg() bindings will fail later at
            # run() when _conn_pg() returns None.
            try:
                pg_conn = self._conn_pg()
                if pg_conn is not None:
                    for name in rwclient.query(pg_conn, _PG_CATALOG_SQL)["name"].to_list():
                        cat[name] = "both" if cat.get(name) == "rw" else "pg"
            except Exception as exc:
                _log.warning(
                    "postgres catalog unavailable — RW-only routing active: %s", exc
                )
            self._catalog = cat
        return self._catalog

    def run(self, spec: QuerySpec) -> pl.DataFrame:
        store = spec.store
        if store == "auto":
            store = decide_store(tables_in(spec.sql), self.catalog())
        conn = self._conn_rw() if store == "rw" else self._conn_pg()
        if conn is None:
            raise RuntimeError(f"no connection for store {store!r} (postgres DSN unset?)")
        return rwclient.query(conn, spec.sql, spec.params)

    def close(self) -> None:
        for c in (self._rw, self._pg):
            try:
                if c is not None:
                    c.close()
            except Exception:
                pass
