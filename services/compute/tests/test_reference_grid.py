"""Forward-fill grid sampler tests."""

from __future__ import annotations

import pytest

from tests.reference.grid import forward_filled_grid


DAY_US = 86_400_000_000


def test_empty_series_all_nones():
    grid = forward_filled_grid(
        series=[], start_us=0, end_us=3 * DAY_US, interval_us=DAY_US,
    )
    assert grid == [(0, None), (DAY_US, None), (2 * DAY_US, None), (3 * DAY_US, None)]


def test_forward_fill_carries_last_value():
    series = [(0, 100.0), (2 * DAY_US, 110.0)]
    grid = forward_filled_grid(
        series=series, start_us=0, end_us=4 * DAY_US, interval_us=DAY_US,
    )
    assert [g[1] for g in grid] == [100.0, 100.0, 110.0, 110.0, 110.0]


def test_grid_inclusive_of_start():
    series = [(0, 50.0)]
    grid = forward_filled_grid(
        series=series, start_us=0, end_us=DAY_US, interval_us=DAY_US,
    )
    assert grid[0] == (0, 50.0)


def test_observation_after_grid_point_not_visible():
    series = [(5 * DAY_US, 100.0)]
    grid = forward_filled_grid(
        series=series, start_us=0, end_us=4 * DAY_US, interval_us=DAY_US,
    )
    assert all(v is None for _, v in grid)


def test_none_observations_skipped():
    series = [(0, 100.0), (DAY_US, None), (2 * DAY_US, 120.0)]
    grid = forward_filled_grid(
        series=series, start_us=0, end_us=3 * DAY_US, interval_us=DAY_US,
    )
    assert [v for _, v in grid] == [100.0, 100.0, 120.0, 120.0]


def test_inverted_window_returns_empty():
    grid = forward_filled_grid(
        series=[(0, 1.0)], start_us=10, end_us=5, interval_us=1,
    )
    assert grid == []


def test_zero_interval_raises():
    with pytest.raises(ValueError):
        forward_filled_grid(
            series=[], start_us=0, end_us=10, interval_us=0,
        )
