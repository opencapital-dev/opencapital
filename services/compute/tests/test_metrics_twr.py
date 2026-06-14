"""Parity tests: compute.metrics.twr vs tests.reference.twr.

Each fixture is expressed as (ts, nav) pairs + flows + window.  Both
implementations receive identical inputs.  We assert that the scalar TWR value
returned by each agrees to floating-point tolerance, and that None cases match
exactly.
"""

from __future__ import annotations

import pytest
import polars as pl

from tests.reference.twr import twr as ref_twr
from compute.metrics.twr import twr as new_twr

DAY = 86_400_000_000  # 1 day in microseconds


def _df(rows: list[tuple[int, float | None]]) -> pl.DataFrame:
    ts_col = [r[0] for r in rows]
    val_col = [r[1] for r in rows]
    return pl.DataFrame({"ts": ts_col, "value": val_col}, schema={"ts": pl.Int64, "value": pl.Float64})


def _ref(
    rows: list[tuple[int, float | None]],
    flows: list[int],
    t0: int,
    t1: int,
) -> float | None:
    value, _ = ref_twr(
        nav_series=rows,
        flow_timestamps_us=flows,
        window_start_us=t0,
        window_end_us=t1,
    )
    return value


def _new(
    rows: list[tuple[int, float | None]],
    flows: list[int],
    t0: int,
    t1: int,
) -> float | None:
    return new_twr(_df(rows), flows, t0, t1)


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

def _assert_parity(
    rows: list[tuple[int, float | None]],
    flows: list[int],
    t0: int,
    t1: int,
) -> None:
    ref = _ref(rows, flows, t0, t1)
    got = _new(rows, flows, t0, t1)
    if ref is None:
        assert got is None, f"expected None but got {got}"
    else:
        assert got == pytest.approx(ref, rel=1e-9, abs=1e-12), (
            f"parity mismatch: ref={ref}, got={got}"
        )


# ---------------------------------------------------------------------------
# fixture cases
# ---------------------------------------------------------------------------

def test_parity_no_flows():
    """Single segment — simple HPR."""
    rows = [(0, 100.0), (10 * DAY, 110.0)]
    _assert_parity(rows, [], 0, 10 * DAY)


def test_parity_one_flow_mid_window():
    """Deposit doubles NAV mid-window; manager earned 10% each half."""
    rows = [
        (0, 100.0),
        (5 * DAY - 1, 110.0),
        (5 * DAY, 220.0),
        (10 * DAY, 242.0),
    ]
    _assert_parity(rows, [5 * DAY], 0, 10 * DAY)


def test_parity_multi_flow_three_segments():
    """Two internal flows, three segments each returning 10%."""
    rows = [
        (0, 100.0),
        (3 * DAY - 1, 110.0),
        (3 * DAY, 210.0),
        (7 * DAY - 1, 231.0),
        (7 * DAY, 131.0),
        (10 * DAY, 144.1),
    ]
    _assert_parity(rows, [3 * DAY, 7 * DAY], 0, 10 * DAY)


def test_parity_flow_at_window_edges_ignored():
    """Flows exactly at t0 and t1 are excluded (strictly inside only)."""
    rows = [(0, 100.0), (10 * DAY, 121.0)]
    _assert_parity(rows, [0, 10 * DAY], 0, 10 * DAY)


def test_parity_unchanged_nav_with_flow_zero_twr():
    """NAV unchanged by manager; deposit at mid-window → zero TWR."""
    rows = [
        (0, 100.0),
        (5 * DAY - 1, 100.0),
        (5 * DAY, 200.0),
        (10 * DAY, 200.0),
    ]
    _assert_parity(rows, [5 * DAY], 0, 10 * DAY)


def test_parity_nav_gap_before_window_start():
    """No NAV at or before t0 → both return None."""
    rows = [(5 * DAY, 100.0), (10 * DAY, 110.0)]
    _assert_parity(rows, [], 0, 10 * DAY)


def test_parity_nav_null_at_boundary_skipped():
    """Null NAV at t0 is skipped; the next valid NAV is used."""
    rows = [(0, None), (1, 100.0), (10 * DAY, 110.0)]
    _assert_parity(rows, [], 0, 10 * DAY)


def test_parity_zero_anchor_nav_returns_none():
    """nav_a == 0 is non-positive → both return None."""
    rows = [(0, 0.0), (10 * DAY, 50.0)]
    _assert_parity(rows, [], 0, 10 * DAY)


def test_parity_negative_anchor_nav_returns_none():
    """nav_a < 0 is non-positive → both return None."""
    rows = [(0, -10.0), (10 * DAY, 50.0)]
    _assert_parity(rows, [], 0, 10 * DAY)


def test_parity_inverted_window_returns_none():
    """t1 <= t0 → both return None."""
    rows = [(0, 100.0), (10 * DAY, 110.0)]
    _assert_parity(rows, [], 10 * DAY, 0)


def test_parity_equal_window_endpoints_returns_none():
    """t0 == t1 → both return None."""
    rows = [(0, 100.0), (10 * DAY, 110.0)]
    _assert_parity(rows, [], 5 * DAY, 5 * DAY)


def test_parity_null_nav_at_internal_flow_boundary():
    """Null NAV at the flow timestamp forces a skip; next valid NAV is used."""
    rows = [
        (0, 100.0),
        (5 * DAY - 1, None),   # null at flow ts; before-lookup skips it
        (5 * DAY - 2, 95.0),   # strictly-before the flow
        (5 * DAY, None),        # null — after lookup must skip forward
        (5 * DAY + 1, 200.0),  # first valid after the flow
        (10 * DAY, 220.0),
    ]
    # Insert rows in ascending ts order
    rows_sorted = sorted(rows, key=lambda r: r[0])
    _assert_parity(rows_sorted, [5 * DAY], 0, 10 * DAY)
