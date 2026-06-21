import polars as pl
from compute.store import rw, pg, Store
from compute.contract import QuerySpec

def test_rw_pg_constructors():
    assert rw("SELECT 1", 5) == QuerySpec("rw", "SELECT 1", (5,))
    assert pg("SELECT 2") == QuerySpec("pg", "SELECT 2", ())

def test_run_routes_explicit_store(monkeypatch):
    calls = []
    s = Store.__new__(Store)          # bypass real connections
    s._rw = object(); s._pg = object()
    s._catalog = {"portfolio_per_tick": "rw"}
    def fake_query(conn, sql, params):
        calls.append((conn, sql, params)); return pl.DataFrame({"x": [1]})
    monkeypatch.setattr("compute.store.rwclient.query", fake_query)
    s.run(QuerySpec("rw", "SELECT 1", ()))
    assert calls[0][0] is s._rw
    s.run(QuerySpec("pg", "SELECT 1", ()))
    assert calls[1][0] is s._pg

def test_catalog_pg_down_still_returns_rw(monkeypatch):
    """A Store whose Postgres connection raises must still return RW-only catalog."""
    rw_conn = object()
    s = Store.__new__(Store)
    s._rw = rw_conn
    s._pg = None
    s._pg_dsn = "postgresql://fake"
    s._catalog = None

    # RW query returns two tables.
    def fake_query(conn, sql, params=()):
        if conn is rw_conn:
            return pl.DataFrame({"name": ["e_nav", "portfolio_per_tick"]})
        raise AssertionError("unexpected query on non-RW conn")

    monkeypatch.setattr("compute.store.rwclient.query", fake_query)

    # _conn_pg raises to simulate an unreachable Postgres.
    def bad_conn_pg(self):
        raise OSError("connection refused")

    monkeypatch.setattr(Store, "_conn_pg", bad_conn_pg)

    cat = s.catalog()
    # Both RW tables must be present; PG absence must not propagate as an error.
    assert cat == {"e_nav": "rw", "portfolio_per_tick": "rw"}
