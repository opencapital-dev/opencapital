"""Unit tests for sample_at_grid and cum_returns_from_navs.

Hand-computed oracles verify parity with the numbat series.numbat definitions:
  sample_at_grid  — asof per grid tick (forward-fill / step-function hold)
  cum_returns_from_navs — v_i / v_0 - 1 rebased at first value
"""

from __future__ import annotations

import polars as pl
import pytest

from compute.metrics.returns import sample_at_grid, cum_returns_from_navs


_D = 86_400_000_000  # 1 day in µs


def _bench_df(rows: list[tuple[int, float]]) -> pl.DataFrame:
    return pl.DataFrame(
        {"ts": [r[0] for r in rows], "value": [r[1] for r in rows]},
        schema={"ts": pl.Int64, "value": pl.Float64},
    )


# ---------------------------------------------------------------------------
# sample_at_grid
# ---------------------------------------------------------------------------

def test_sample_at_grid_forward_fill():
    navs = _bench_df([(0, 100.0), (2 * _D, 110.0), (4 * _D, 115.0)])
    grid = [0, _D, 2 * _D, 3 * _D, 4 * _D]
    result = sample_at_grid(navs, grid)
    assert result == [100.0, 100.0, 110.0, 110.0, 115.0]


def test_sample_at_grid_exact_hits():
    navs = _bench_df([(0, 50.0), (_D, 60.0), (2 * _D, 70.0)])
    grid = [0, _D, 2 * _D]
    result = sample_at_grid(navs, grid)
    assert result == [50.0, 60.0, 70.0]


def test_sample_at_grid_before_first_point_is_none():
    navs = _bench_df([(_D, 100.0), (2 * _D, 110.0)])
    grid = [0, _D, 2 * _D]
    result = sample_at_grid(navs, grid)
    assert result[0] is None
    assert result[1] == pytest.approx(100.0)
    assert result[2] == pytest.approx(110.0)


def test_sample_at_grid_empty_navs():
    navs = _bench_df([])
    grid = [0, _D]
    result = sample_at_grid(navs, grid)
    assert result == [None, None]


def test_sample_at_grid_empty_grid():
    navs = _bench_df([(0, 100.0)])
    result = sample_at_grid(navs, [])
    assert result == []


# ---------------------------------------------------------------------------
# cum_returns_from_navs
# ---------------------------------------------------------------------------

def test_cum_returns_from_navs_basic():
    values = [100.0, 110.0, 105.0, 120.0]
    result = cum_returns_from_navs(values)
    assert result[0] == pytest.approx(0.0)
    assert result[1] == pytest.approx(0.10)
    assert result[2] == pytest.approx(0.05)
    assert result[3] == pytest.approx(0.20)


def test_cum_returns_from_navs_none_in_middle():
    values = [100.0, None, 120.0]
    result = cum_returns_from_navs(values)
    assert result[0] == pytest.approx(0.0)
    assert result[1] is None
    assert result[2] == pytest.approx(0.20)


def test_cum_returns_from_navs_none_anchor():
    values = [None, 100.0, 110.0]
    result = cum_returns_from_navs(values)
    assert all(v is None for v in result)


def test_cum_returns_from_navs_zero_anchor():
    values = [0.0, 100.0, 110.0]
    result = cum_returns_from_navs(values)
    assert all(v is None for v in result)


def test_cum_returns_from_navs_empty():
    assert cum_returns_from_navs([]) == []


def test_cum_returns_from_navs_single():
    result = cum_returns_from_navs([200.0])
    assert result == [pytest.approx(0.0)]


def test_cum_returns_from_navs_all_none_after_asof_gap():
    values = [None, 100.0, 110.0]
    result = cum_returns_from_navs(values)
    assert result == [None, None, None]
