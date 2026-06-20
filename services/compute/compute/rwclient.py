"""Direct RisingWave pgwire client — run SQL, return a polars frame.

pg8000 is pure-Python (PyInstaller-freeze-friendly). RisingWave speaks the
Postgres wire protocol, so the simple-query path works unchanged. Replaces the
read-gateway HTTP hop (compute/gateway.py).
"""
from __future__ import annotations

import logging
from urllib.parse import urlparse

import pg8000.native
import polars as pl

log = logging.getLogger("compute.rwclient")


def connect(dsn: str) -> pg8000.native.Connection:
    """Open a pg8000 connection from a postgres:// DSN (loopback, trust auth)."""
    u = urlparse(dsn)
    return pg8000.native.Connection(
        user=u.username or "root",
        password=u.password or None,
        host=u.hostname or "127.0.0.1",
        port=u.port or 4566,
        database=(u.path or "/dev").lstrip("/") or "dev",
    )


def query(conn: pg8000.native.Connection, sql: str, params: tuple = ()) -> pl.DataFrame:
    """Run *sql* with positional *params* ($1,$2,…) and return a polars frame."""
    rows = conn.run(sql, *params)
    columns = [c["name"] for c in conn.columns]
    return _frame_from(columns, rows)


def _frame_from(columns: list[str], rows: list[list]) -> pl.DataFrame:
    """Build a polars frame from column names + row arrays in column order.

    Each column built non-strictly so mixed int/float coerces to a common dtype;
    `ts` forced to Int64; empty rows yield a typed zero-height frame.
    """
    series = []
    for i, name in enumerate(columns):
        vals = [row[i] for row in rows]
        dtype = pl.Int64 if name == "ts" else None
        series.append(pl.Series(name, vals, dtype=dtype, strict=False))
    return pl.DataFrame(series)
