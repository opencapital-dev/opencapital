import polars as pl
from compute.rwclient import _frame_from


def test_frame_forces_ts_int64_and_preserves_nulls():
    df = _frame_from(["ts", "nav"], [[1000, 1.5], [2000, None]])
    assert df.schema["ts"] == pl.Int64
    assert df["nav"].to_list() == [1.5, None]


def test_empty_rows_yield_typed_zero_height_frame():
    df = _frame_from(["ts", "nav"], [])
    assert df.height == 0
    assert df.schema["ts"] == pl.Int64


def test_query_translates_positional_to_named_params():
    """rwclient.query must translate $N -> :pN and pass kwargs (pg8000.native
    binds named params, not positional). Regression for the multi-param /
    int-param breakage found in T10."""
    from compute import rwclient

    class FakeConn:
        columns = [{"name": "a"}]
        def __init__(self): self.called = None
        def run(self, sql, **kw): self.called = (sql, kw); return [[1]]

    c = FakeConn()
    rwclient.query(c, "SELECT a FROM t WHERE x=$1 AND y=$2", ("p", 0))
    sql, kw = c.called
    assert ":p1" in sql and ":p2" in sql and "$1" not in sql
    assert kw == {"p1": "p", "p2": 0}


def test_query_preserves_cast_operator():
    """The $N translation must not mangle the `::` cast operator."""
    from compute import rwclient

    class FakeConn:
        columns = [{"name": "a"}]
        def run(self, sql, **kw): self.sql = sql; return []

    c = FakeConn()
    rwclient.query(c, "SELECT portfolio_id::text FROM t WHERE p=$1", ("x",))
    assert "::text" in c.sql and ":p1" in c.sql
