"""Drawdown metrics over a cumulative-return list.

All functions operate on ``cum``: a ``list[float | None]`` of cumulative
returns (fractional; 0.10 = +10% from anchor).  The equity curve is
``eq_i = 1 + cum_i``.

Sign convention (matching numbat stdlib/stats.numbat):
  - max_drawdown   — most negative peak-to-trough ratio, e.g. -0.20 for a 20% drop.
  - current_drawdown — drawdown at the final point; 0 if at or above running peak.

Public API
----------
max_drawdown(cum)           — scalar, 0 when curve never drops below running peak
current_drawdown(cum)       — scalar, 0 when last point >= running peak
drawdown_episodes(cum, grid) — list of DrawdownEpisode dicts, one per completed cycle
"""

from __future__ import annotations

from dataclasses import dataclass


# ---------------------------------------------------------------------------
# Scalar drawdown helpers
# ---------------------------------------------------------------------------

def _valid_cum(cum: list[float | None]) -> list[float]:
    return [c for c in cum if c is not None]


def max_drawdown(cum: list[float | None]) -> float | None:
    """Most negative peak-to-trough ratio over the cumulative return series.

    Returns 0 when the series is empty or never drops below its running peak.
    Returns None when all values are None.
    """
    vals = _valid_cum(cum)
    if not vals:
        return None
    peak = 1.0 + vals[0]
    min_dd = 0.0
    for c in vals[1:]:
        eq = 1.0 + c
        if eq >= peak:
            peak = eq
        else:
            dd = eq / peak - 1.0
            if dd < min_dd:
                min_dd = dd
    return min_dd


def current_drawdown(cum: list[float | None]) -> float | None:
    """Drawdown at the last point relative to the running peak.

    Returns 0 when the last point is at or above the running peak.
    Returns None when the series is empty or all values are None.
    """
    vals = _valid_cum(cum)
    if not vals:
        return None
    peak = 1.0 + vals[0]
    for c in vals[1:]:
        eq = 1.0 + c
        if eq > peak:
            peak = eq
    last_eq = 1.0 + vals[-1]
    if last_eq >= peak:
        return 0.0
    return last_eq / peak - 1.0


# ---------------------------------------------------------------------------
# Episode enumeration
# ---------------------------------------------------------------------------

@dataclass(slots=True)
class DrawdownEpisode:
    """One peak -> trough -> (optional) recovery cycle.

    ``recovery_ts`` and ``time_to_recover_sec`` are None for unrecovered
    episodes still underwater at the window end.
    """
    peak_ts: int
    trough_ts: int
    recovery_ts: int | None
    depth: float
    time_to_trough_sec: float
    time_to_recover_sec: float | None


def drawdown_episodes(
    cum: list[float | None],
    grid: list[int],
) -> list[DrawdownEpisode]:
    """Every peak -> trough -> (optional) recovery cycle on the equity curve.

    ``cum[i]`` is the cumulative return at ``grid[i]`` (int-µs timestamps).
    Episodes are returned in chronological peak order.
    A trailing episode still below its peak at the end gets
    ``recovery_ts=None``, ``time_to_recover_sec=None``.
    """
    assert len(cum) == len(grid)
    if not grid:
        return []

    _US = 1_000_000  # microseconds per second

    peak_val = 1.0 + (cum[0] if cum[0] is not None else 0.0)
    peak_ts = grid[0]

    is_open = False
    open_peak_ts = grid[0]
    trough_ts = grid[0]
    trough_depth = 0.0
    episodes: list[DrawdownEpisode] = []

    for i in range(1, len(grid)):
        c = cum[i]
        if c is None:
            continue
        eq = 1.0 + c
        ts = grid[i]

        if eq >= peak_val:
            if is_open:
                episodes.append(DrawdownEpisode(
                    peak_ts=open_peak_ts,
                    trough_ts=trough_ts,
                    recovery_ts=ts,
                    depth=trough_depth,
                    time_to_trough_sec=(trough_ts - open_peak_ts) / _US,
                    time_to_recover_sec=(ts - open_peak_ts) / _US,
                ))
                is_open = False
            peak_val = eq
            peak_ts = ts
        else:
            dd = eq / peak_val - 1.0
            if not is_open:
                is_open = True
                open_peak_ts = peak_ts
                trough_ts = ts
                trough_depth = dd
            elif dd < trough_depth:
                trough_ts = ts
                trough_depth = dd

    if is_open:
        episodes.append(DrawdownEpisode(
            peak_ts=open_peak_ts,
            trough_ts=trough_ts,
            recovery_ts=None,
            depth=trough_depth,
            time_to_trough_sec=(trough_ts - open_peak_ts) / _US,
            time_to_recover_sec=None,
        ))

    return episodes
