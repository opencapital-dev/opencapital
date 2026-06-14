"""Forward-fill grid sampler.

Given an irregular ``(ts_us, value)`` series, sample at a uniform
interval over ``[start_us, end_us]``, carrying the last observed value
forward at each grid point.
"""

from __future__ import annotations

import bisect


def forward_filled_grid(
    *,
    series: list[tuple[int, float | None]],
    start_us: int,
    end_us: int,
    interval_us: int,
) -> list[tuple[int, float | None]]:
    """Sample ``series`` at ``start_us, start_us+interval_us, ..., <= end_us``.

    Each grid point gets the last observation with ``event_ts <= grid_ts``
    (forward-fill). ``None`` when no prior observation exists.
    """
    if interval_us <= 0:
        raise ValueError(f"interval_us must be positive, got {interval_us}")
    if end_us < start_us:
        return []
    n_points = (end_us - start_us) // interval_us + 1
    grid_ts = [start_us + i * interval_us for i in range(n_points)]
    if not series:
        return [(ts, None) for ts in grid_ts]
    timestamps = [t for t, _ in series]
    out: list[tuple[int, float | None]] = []
    for ts in grid_ts:
        idx = bisect.bisect_right(timestamps, ts) - 1
        value: float | None = None
        while idx >= 0:
            v = series[idx][1]
            if v is not None:
                value = v
                break
            idx -= 1
        out.append((ts, value))
    return out
