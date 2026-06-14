"""Parity tests: compute.metrics.returns vs tests.reference reference implementations.

Grid builder → tests.reference.grid.forward_filled_grid (timestamps only).
cumulative_simple_return → tests.reference.twr.cumulative_simple_return_grid.
cumulative_twr → tests.reference.twr.cumulative_twr_grid.

Each parity fixture feeds identical data to both sides and asserts that the
outputs agree to floating-point tolerance.
"""

from __future__ import annotations

import pytest
import polars as pl

from tests.reference.grid import forward_filled_grid as ref_grid
from tests.reference.twr import (
    cumulative_simple_return_grid as ref_simple,
    cumulative_twr_grid as ref_ctwr,
)
from compute.metrics.returns import build_grid, cumulative_simple_return, cumulative_twr

DAY = 86_400_000_000  # microseconds


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

def _df(rows: list[tuple[int, float | None]]) -> pl.DataFrame:
    return pl.DataFrame(
        {"ts": [r[0] for r in rows], "value": [r[1] for r in rows]},
        schema={"ts": pl.Int64, "value": pl.Float64},
    )


def _assert_series_parity(
    got: list[tuple[int, float | None]],
    ref: list[tuple[int, float | None]],
) -> None:
    assert len(got) == len(ref), f"length mismatch: got {len(got)}, ref {len(ref)}"
    for i, ((gt, gv), (rt, rv)) in enumerate(zip(got, ref)):
        assert gt == rt, f"[{i}] timestamp mismatch: got {gt}, ref {rt}"
        if rv is None:
            assert gv is None, f"[{i}] expected None but got {gv}"
        else:
            assert gv is not None, f"[{i}] expected {rv} but got None"
            assert gv == pytest.approx(rv, rel=1e-9, abs=1e-12), (
                f"[{i}] value mismatch: got {gv}, ref {rv}"
            )


# ---------------------------------------------------------------------------
# build_grid parity
# ---------------------------------------------------------------------------
# Reference: forward_filled_grid produces grid points as start_us + i*interval_us
# for i in range((end_us - start_us) // interval_us + 1).  We test that
# build_grid returns exactly those timestamps (ignoring the forward-fill values).

def _ref_grid_ts(t0: int, t1: int, interval: int) -> list[int]:
    """Extract just the timestamps from the reference forward-fill grid."""
    return [ts for ts, _ in ref_grid(series=[], start_us=t0, end_us=t1, interval_us=interval)]


def test_grid_single_point():
    """t0 == t1 → one-element grid."""
    t0 = 0
    t1 = 0
    step = DAY
    assert build_grid(t0, t1, step) == _ref_grid_ts(t0, t1, step)


def test_grid_exact_multiple():
    """t1 is an exact multiple of step beyond t0 → last point == t1."""
    t0 = 0
    t1 = 5 * DAY
    step = DAY
    got = build_grid(t0, t1, step)
    ref = _ref_grid_ts(t0, t1, step)
    assert got == ref
    assert got[-1] == t1  # end is inclusive


def test_grid_partial_last_step():
    """t1 is not a multiple of step → last point < t1 (no overshoot)."""
    t0 = 0
    t1 = 5 * DAY + DAY // 3
    step = DAY
    assert build_grid(t0, t1, step) == _ref_grid_ts(t0, t1, step)


def test_grid_inverted_range():
    """t1 < t0 → empty grid."""
    assert build_grid(10 * DAY, 0, DAY) == []
    assert _ref_grid_ts(10 * DAY, 0, DAY) == []


def test_grid_string_day():
    """String ``"1d"`` == DAY microseconds."""
    t0 = 0
    t1 = 7 * DAY
    assert build_grid(t0, t1, "1d") == _ref_grid_ts(t0, t1, DAY)


def test_grid_string_hour():
    """String ``"1h"`` == 3_600_000_000 microseconds."""
    HOUR = 3_600_000_000
    t0 = 0
    t1 = 24 * HOUR
    assert build_grid(t0, t1, "1h") == _ref_grid_ts(t0, t1, HOUR)


def test_grid_string_week():
    """String ``"1w"`` == 7 days in microseconds."""
    WEEK = 7 * DAY
    t0 = 0
    t1 = 4 * WEEK
    assert build_grid(t0, t1, "1w") == _ref_grid_ts(t0, t1, WEEK)


def test_grid_raw_int_step():
    """Raw int step passes through unchanged."""
    t0 = 1_000
    t1 = 10_000
    step = 3_000
    assert build_grid(t0, t1, step) == _ref_grid_ts(t0, t1, step)


def test_grid_invalid_step_raises():
    with pytest.raises(ValueError):
        build_grid(0, DAY, 0)
    with pytest.raises(ValueError):
        build_grid(0, DAY, "5x")


# ---------------------------------------------------------------------------
# cumulative_simple_return parity
# ---------------------------------------------------------------------------

def _parity_simple(
    rows: list[tuple[int, float | None]],
    start: int,
    grid: list[int],
) -> None:
    ref = ref_simple(series=rows, start_us=start, grid_ts_us=grid)
    got = cumulative_simple_return(_df(rows), start, grid)
    _assert_series_parity(got, ref)


def test_simple_basic():
    """Nav rises linearly — verify rebased return at each grid point."""
    rows = [(0, 100.0), (DAY, 110.0), (2 * DAY, 121.0)]
    grid = build_grid(0, 2 * DAY, DAY)
    _parity_simple(rows, 0, grid)


def test_simple_missing_start():
    """No NAV at or before start → all None."""
    rows = [(DAY, 100.0), (2 * DAY, 110.0)]
    grid = build_grid(0, 2 * DAY, DAY)
    _parity_simple(rows, 0, grid)


def test_simple_zero_start():
    """Start NAV == 0 → all None."""
    rows = [(0, 0.0), (DAY, 100.0)]
    grid = build_grid(0, 2 * DAY, DAY)
    _parity_simple(rows, 0, grid)


def test_simple_negative_start():
    """Start NAV < 0 → all None."""
    rows = [(0, -50.0), (DAY, 100.0)]
    grid = build_grid(0, DAY, DAY)
    _parity_simple(rows, 0, grid)


def test_simple_null_at_start_skipped():
    """Null at t=0 skipped; earlier non-null used as start."""
    rows = [(0, None), (1, 100.0), (2 * DAY, 120.0)]
    grid = build_grid(0, 2 * DAY, DAY)
    _parity_simple(rows, 0, grid)


def test_simple_null_mid_series():
    """Null at a grid point → that point is None, others are fine."""
    rows = [(0, 100.0), (DAY, None), (2 * DAY, 110.0)]
    grid = build_grid(0, 2 * DAY, DAY)
    _parity_simple(rows, 0, grid)


def test_simple_empty_series():
    """Empty NAV series → all None."""
    rows: list[tuple[int, float | None]] = []
    grid = build_grid(0, 3 * DAY, DAY)
    _parity_simple(rows, 0, grid)


def test_simple_single_point_grid():
    """Grid with one point (t0 == t1)."""
    rows = [(0, 100.0), (DAY, 150.0)]
    grid = build_grid(0, 0, DAY)
    _parity_simple(rows, 0, grid)


# ---------------------------------------------------------------------------
# cumulative_twr parity
# ---------------------------------------------------------------------------

def _parity_ctwr(
    rows: list[tuple[int, float | None]],
    flows: list[int],
    start: int,
    grid: list[int],
) -> None:
    ref = ref_ctwr(
        nav_series=rows,
        flow_timestamps_us=flows,
        start_us=start,
        grid_ts_us=grid,
    )
    got = cumulative_twr(_df(rows), flows, start, grid)
    _assert_series_parity(got, ref)


def test_ctwr_no_flows():
    """No flows — single segment, simple HPR chain."""
    rows = [(0, 100.0), (5 * DAY, 110.0), (10 * DAY, 121.0)]
    grid = build_grid(0, 10 * DAY, 5 * DAY)
    _parity_ctwr(rows, [], 0, grid)


def test_ctwr_one_flow():
    """One deposit mid-window; manager earned 10% each half."""
    rows = [
        (0, 100.0),
        (5 * DAY - 1, 110.0),
        (5 * DAY, 220.0),
        (10 * DAY, 242.0),
    ]
    grid = build_grid(0, 10 * DAY, DAY)
    _parity_ctwr(rows, [5 * DAY], 0, grid)


def test_ctwr_two_flows():
    """Two flows — three segments each returning 10%."""
    rows = [
        (0, 100.0),
        (3 * DAY - 1, 110.0),
        (3 * DAY, 210.0),
        (7 * DAY - 1, 231.0),
        (7 * DAY, 131.0),
        (10 * DAY, 144.1),
    ]
    grid = build_grid(0, 10 * DAY, DAY)
    _parity_ctwr(rows, [3 * DAY, 7 * DAY], 0, grid)


def test_ctwr_missing_start_anchor():
    """No NAV at or before start → all None."""
    rows = [(DAY, 100.0), (5 * DAY, 120.0)]
    grid = build_grid(0, 5 * DAY, DAY)
    _parity_ctwr(rows, [], 0, grid)


def test_ctwr_zero_anchor():
    """Anchor NAV == 0 → all None."""
    rows = [(0, 0.0), (DAY, 100.0)]
    grid = build_grid(0, DAY, DAY)
    _parity_ctwr(rows, [], 0, grid)


def test_ctwr_null_nav_at_flow():
    """Null NAV at flow boundary — before/after lookups skip nulls."""
    rows = [
        (0, 100.0),
        (5 * DAY - 2, 95.0),
        (5 * DAY - 1, None),  # null; before-lookup skips back to 95
        (5 * DAY, None),       # null; after-lookup skips forward
        (5 * DAY + 1, 200.0),
        (10 * DAY, 220.0),
    ]
    rows_sorted = sorted(rows, key=lambda r: r[0])
    grid = build_grid(0, 10 * DAY, DAY)
    _parity_ctwr(rows_sorted, [5 * DAY], 0, grid)


def test_ctwr_null_mid_grid():
    """Null NAV at a grid timestamp → that point is None."""
    rows = [(0, 100.0), (DAY, None), (2 * DAY, 120.0)]
    grid = build_grid(0, 2 * DAY, DAY)
    _parity_ctwr(rows, [], 0, grid)


def test_ctwr_flows_at_window_edges_ignored():
    """Flows exactly at start are not counted as internal flows."""
    rows = [(0, 100.0), (5 * DAY, 110.0), (10 * DAY, 121.0)]
    grid = build_grid(0, 10 * DAY, 5 * DAY)
    _parity_ctwr(rows, [0, 10 * DAY], 0, grid)  # edge flows excluded


def test_ctwr_empty_grid():
    """Empty grid → empty output."""
    rows = [(0, 100.0), (DAY, 110.0)]
    _parity_ctwr(rows, [], 0, [])


def test_ctwr_single_point_grid():
    """Grid with one point at start."""
    rows = [(0, 100.0), (DAY, 110.0)]
    grid = build_grid(0, 0, DAY)
    _parity_ctwr(rows, [], 0, grid)


def test_ctwr_dense_grid_daily_over_month():
    """30-day daily grid, two flows — exercises epoch transitions on every day."""
    rows = (
        [(0, 1000.0)]
        + [(i * DAY, 1000.0 + i * 10) for i in range(1, 10)]
        + [(10 * DAY - 1, 1090.0), (10 * DAY, 2090.0)]    # flow 1
        + [(i * DAY, 2090.0 + (i - 10) * 20) for i in range(11, 20)]
        + [(20 * DAY - 1, 2270.0), (20 * DAY, 1270.0)]    # flow 2
        + [(i * DAY, 1270.0 + (i - 20) * 5) for i in range(21, 31)]
    )
    rows_sorted = sorted(rows, key=lambda r: r[0])
    grid = build_grid(0, 30 * DAY, DAY)
    _parity_ctwr(rows_sorted, [10 * DAY, 20 * DAY], 0, grid)
