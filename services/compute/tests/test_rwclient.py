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
