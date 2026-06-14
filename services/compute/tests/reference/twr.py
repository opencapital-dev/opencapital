"""Time-Weighted Return.

TWR is the GIPS-standard "manager performance" metric: portfolio return
neutralised against the timing of external cash flows.

Algorithm:

    boundaries = [window_start, *flows_in_window, window_end]
    for each segment [t_i, t_{i+1}]:
        nav_a = NAV just AFTER t_i
        nav_b = NAV just BEFORE t_{i+1}
        r_i = nav_b / nav_a - 1
    TWR = prod(1 + r_i) - 1

Returns ``(None, partial_segments)`` when any anchor NAV is missing or
non-positive.
"""

from __future__ import annotations

import bisect
from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class TwrSegment:
    """One sub-period in the TWR chain."""

    start_us: int
    end_us: int
    nav_start: float
    nav_end: float
    fractional_return: float


def twr(
    *,
    nav_series: list[tuple[int, float | None]],
    flow_timestamps_us: list[int],
    window_start_us: int,
    window_end_us: int,
) -> tuple[float | None, list[TwrSegment]]:
    """Compute time-weighted return over ``[window_start_us, window_end_us]``."""
    if window_end_us <= window_start_us:
        return None, []
    flows_in = sorted(
        f for f in flow_timestamps_us
        if window_start_us < f < window_end_us
    )
    boundaries = [window_start_us, *flows_in, window_end_us]
    segments: list[TwrSegment] = []
    chain = 1.0
    for i in range(len(boundaries) - 1):
        seg_start = boundaries[i]
        seg_end = boundaries[i + 1]

        if i == 0:
            nav_a = _nav_at_or_before(nav_series, seg_start)
        else:
            nav_a = _nav_at_or_after(nav_series, seg_start)

        if i == len(boundaries) - 2:
            nav_b = _nav_at_or_before(nav_series, seg_end)
        else:
            nav_b = _nav_strictly_before(nav_series, seg_end)

        if nav_a is None or nav_b is None or nav_a <= 0:
            return None, segments
        r = nav_b / nav_a - 1.0
        chain *= (1.0 + r)
        segments.append(TwrSegment(
            start_us=seg_start, end_us=seg_end,
            nav_start=nav_a, nav_end=nav_b,
            fractional_return=r,
        ))
    return chain - 1.0, segments


def cumulative_twr_grid(
    *,
    nav_series: list[tuple[int, float | None]],
    flow_timestamps_us: list[int],
    start_us: int,
    grid_ts_us: list[int],
) -> list[tuple[int, float | None]]:
    """Cumulative TWR at every grid timestamp, rebased at ``start_us``."""
    flows = sorted(f for f in flow_timestamps_us if f > start_us)
    seg_anchor_nav = _nav_at_or_before(nav_series, start_us)
    closed_chain = 1.0
    flow_idx = 0
    out: list[tuple[int, float | None]] = []
    for grid_ts in grid_ts_us:
        while flow_idx < len(flows) and flows[flow_idx] <= grid_ts:
            f = flows[flow_idx]
            nav_pre = _nav_strictly_before(nav_series, f)
            nav_post = _nav_at_or_after(nav_series, f)
            if (
                seg_anchor_nav is None
                or seg_anchor_nav <= 0
                or nav_pre is None
            ):
                seg_anchor_nav = None
                flow_idx += 1
                continue
            seg_return = nav_pre / seg_anchor_nav - 1.0
            closed_chain *= (1.0 + seg_return)
            seg_anchor_nav = nav_post
            flow_idx += 1
        nav_at_ts = _nav_at_or_before(nav_series, grid_ts)
        if seg_anchor_nav is None or seg_anchor_nav <= 0 or nav_at_ts is None:
            out.append((grid_ts, None))
            continue
        partial = nav_at_ts / seg_anchor_nav - 1.0
        out.append((grid_ts, closed_chain * (1.0 + partial) - 1.0))
    return out


def cumulative_simple_return_grid(
    *,
    series: list[tuple[int, float | None]],
    start_us: int,
    grid_ts_us: list[int],
) -> list[tuple[int, float | None]]:
    """Close-to-close cumulative return rebased at ``start_us``."""
    start_value = _nav_at_or_before(series, start_us)
    if start_value is None or start_value <= 0:
        return [(t, None) for t in grid_ts_us]
    out: list[tuple[int, float | None]] = []
    for ts in grid_ts_us:
        v = _nav_at_or_before(series, ts)
        out.append((ts, v / start_value - 1.0 if v is not None else None))
    return out


def _nav_at_or_before(
    series: list[tuple[int, float | None]], ts_us: int,
) -> float | None:
    if not series:
        return None
    timestamps = [t for t, _ in series]
    idx = bisect.bisect_right(timestamps, ts_us) - 1
    while idx >= 0:
        nav = series[idx][1]
        if nav is not None:
            return nav
        idx -= 1
    return None


def _nav_strictly_before(
    series: list[tuple[int, float | None]], ts_us: int,
) -> float | None:
    if not series:
        return None
    timestamps = [t for t, _ in series]
    idx = bisect.bisect_left(timestamps, ts_us) - 1
    while idx >= 0:
        nav = series[idx][1]
        if nav is not None:
            return nav
        idx -= 1
    return None


def _nav_at_or_after(
    series: list[tuple[int, float | None]], ts_us: int,
) -> float | None:
    if not series:
        return None
    timestamps = [t for t, _ in series]
    idx = bisect.bisect_left(timestamps, ts_us)
    while idx < len(series):
        nav = series[idx][1]
        if nav is not None:
            return nav
        idx += 1
    return None
