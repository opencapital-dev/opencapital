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
