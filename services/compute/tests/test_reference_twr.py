"""TWR helper tests."""

from __future__ import annotations

import math

import pytest

from tests.reference.twr import twr


DAY_US = 86_400_000_000


def _nav(ts: int, v: float | None) -> tuple[int, float | None]:
    return ts, v


def test_no_flows_reduces_to_simple_hpr():
    series = [_nav(0, 100.0), _nav(10 * DAY_US, 110.0)]
    rate, segments = twr(
        nav_series=series, flow_timestamps_us=[],
        window_start_us=0, window_end_us=10 * DAY_US,
    )
    assert rate == pytest.approx(0.10)
    assert len(segments) == 1


def test_mid_window_deposit_neutralized():
    flow_ts = 5 * DAY_US
    series = [
        _nav(0,             100.0),
        _nav(5 * DAY_US - 1, 110.0),
        _nav(5 * DAY_US,     220.0),
        _nav(10 * DAY_US,    242.0),
    ]
    rate, segments = twr(
        nav_series=series, flow_timestamps_us=[flow_ts],
        window_start_us=0, window_end_us=10 * DAY_US,
    )
    assert rate == pytest.approx(0.21)
    assert len(segments) == 2
    assert segments[0].fractional_return == pytest.approx(0.10)
    assert segments[1].fractional_return == pytest.approx(0.10)


def test_unchanged_nav_with_flow_yields_zero_twr():
    flow_ts = 5 * DAY_US
    series = [
        _nav(0,                100.0),
        _nav(5 * DAY_US - 1,   100.0),
        _nav(5 * DAY_US,       200.0),
        _nav(10 * DAY_US,      200.0),
    ]
    rate, _ = twr(
        nav_series=series, flow_timestamps_us=[flow_ts],
        window_start_us=0, window_end_us=10 * DAY_US,
    )
    assert rate == pytest.approx(0.0, abs=1e-9)


def test_anchor_missing_returns_none():
    series = [_nav(5 * DAY_US, 100.0), _nav(10 * DAY_US, 110.0)]
    rate, segments = twr(
        nav_series=series, flow_timestamps_us=[],
        window_start_us=0, window_end_us=10 * DAY_US,
    )
    assert rate is None
    assert segments == []


def test_non_positive_anchor_returns_none():
    series = [_nav(0, 0.0), _nav(10 * DAY_US, 50.0)]
    rate, _ = twr(
        nav_series=series, flow_timestamps_us=[],
        window_start_us=0, window_end_us=10 * DAY_US,
    )
    assert rate is None


def test_inverted_window_returns_none():
    series = [_nav(0, 100.0), _nav(10 * DAY_US, 110.0)]
    rate, segments = twr(
        nav_series=series, flow_timestamps_us=[],
        window_start_us=10 * DAY_US, window_end_us=0,
    )
    assert rate is None
    assert segments == []


def test_flows_at_window_edges_ignored():
    series = [_nav(0, 100.0), _nav(10 * DAY_US, 121.0)]
    rate, segments = twr(
        nav_series=series, flow_timestamps_us=[0, 10 * DAY_US],
        window_start_us=0, window_end_us=10 * DAY_US,
    )
    assert len(segments) == 1
    assert rate == pytest.approx(0.21)


def test_two_internal_flows_three_segments():
    series = [
        _nav(0,                100.0),
        _nav(3 * DAY_US - 1,   110.0),
        _nav(3 * DAY_US,       210.0),
        _nav(7 * DAY_US - 1,   231.0),
        _nav(7 * DAY_US,       131.0),
        _nav(10 * DAY_US,      144.1),
    ]
    rate, segments = twr(
        nav_series=series,
        flow_timestamps_us=[3 * DAY_US, 7 * DAY_US],
        window_start_us=0, window_end_us=10 * DAY_US,
    )
    assert len(segments) == 3
    expected = math.prod(1 + s.fractional_return for s in segments) - 1
    assert rate == pytest.approx(expected)
    for s in segments:
        assert s.fractional_return == pytest.approx(0.10, rel=1e-3)
