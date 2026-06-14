"""Cumulative return series and timestamp grid builder.

Builds on the T2 NAV lookups (``asof``, ``before``, ``after``) to produce
equity-curve grids for visualisation.

Public API
----------
build_grid(t0, t1, step)                      — uniform timestamp grid
cumulative_simple_return(navs, start, grid)   — close-to-close rebased series
cumulative_twr(navs, flows, start, grid)      — TWR equity-curve via epoch scan
sample_at_grid(navs, grid)                    — benchmark price at each grid tick (asof)
cum_returns_from_navs(values)                 — simple cumulative return rebased at first value
"""

from __future__ import annotations

import bisect
from dataclasses import dataclass

import polars as pl

from compute.metrics.twr import asof, before, after

# ---------------------------------------------------------------------------
# Duration helpers
# ---------------------------------------------------------------------------

_DURATION_US: dict[str, int] = {
    "m": 60_000_000,
    "h": 3_600_000_000,
    "d": 86_400_000_000,
    "w": 7 * 86_400_000_000,
}


def _parse_step(step: int | str) -> int:
    """Convert a human step string (``"1d"``, ``"4h"``) or raw int to microseconds."""
    if isinstance(step, int):
        return step
    step = step.strip()
    unit = _DURATION_US.get(step[-1:])
    if unit is None:
        raise ValueError(f"unrecognised step: {step!r}")
    return int(step[:-1]) * unit


# ---------------------------------------------------------------------------
# Grid
# ---------------------------------------------------------------------------

def build_grid(t0: int, t1: int, step: int | str) -> list[int]:
    """Uniform timestamp grid over ``[t0, t1]`` at *step* intervals.

    Points: ``t0, t0+step, t0+2*step, ...`` while ``<= t1``.
    End is inclusive; the last point is the largest ``t0 + i*step <= t1``.

    *step* may be a raw microsecond integer or a string like ``"1d"``, ``"4h"``.
    Raises ``ValueError`` when ``step <= 0`` or the string is unrecognised.
    """
    interval = _parse_step(step)
    if interval <= 0:
        raise ValueError(f"step must be positive, got {step!r}")
    if t1 < t0:
        return []
    n = (t1 - t0) // interval + 1
    return [t0 + i * interval for i in range(n)]


# ---------------------------------------------------------------------------
# Simple cumulative return
# ---------------------------------------------------------------------------

def cumulative_simple_return(
    navs: pl.DataFrame,
    start: int,
    grid: list[int],
) -> list[tuple[int, float | None]]:
    """Close-to-close cumulative return series rebased at *start* (microseconds).

    Each grid point reports ``nav_at_ts / nav_at_start - 1``, where both
    lookups use at-or-before semantics.  The whole series is ``None``-filled
    when the start value is missing or non-positive.
    """
    start_value = asof(navs, start)
    if start_value is None or start_value <= 0:
        return [(ts, None) for ts in grid]
    return [
        (ts, (v / start_value - 1.0) if (v := asof(navs, ts)) is not None else None)
        for ts in grid
    ]


# ---------------------------------------------------------------------------
# Cumulative TWR — functional epoch scan
# ---------------------------------------------------------------------------

@dataclass(frozen=True, slots=True)
class _Epoch:
    """A segment of the timeline between two consecutive flows.

    ``closed_chain`` is the product of all sub-period (1+r) factors that
    closed *before* this epoch started.  ``anchor`` is the post-flow NAV
    that opens this epoch (or the initial at-or-before NAV for epoch 0).
    ``None`` anchor means all subsequent grid points must emit None.
    """
    closed_chain: float
    anchor: float | None


def _build_epochs(navs: pl.DataFrame, flows: list[int], start: int) -> list[tuple[int, _Epoch]]:
    """Pre-compute one ``(epoch_start_ts, Epoch)`` entry per segment.

    Segments are ``[start, flows[0]), [flows[0], flows[1]), ..., [flows[-1], inf)``.
    Each entry records the closed product and the anchor NAV at the start of
    that epoch so that grid-point evaluation is a pure lookup.
    """
    ordered = sorted(f for f in flows if f > start)
    epochs: list[tuple[int, _Epoch]] = []
    chain = 1.0
    anchor = asof(navs, start)

    # Epoch 0 — opens at start
    epochs.append((start, _Epoch(closed_chain=chain, anchor=anchor)))

    for flow_ts in ordered:
        if anchor is None or anchor <= 0:
            # Chain is broken; mark and propagate a None anchor forward.
            epochs.append((flow_ts, _Epoch(closed_chain=chain, anchor=None)))
            anchor = None
            continue
        nav_pre = before(navs, flow_ts)
        if nav_pre is None:
            epochs.append((flow_ts, _Epoch(closed_chain=chain, anchor=None)))
            anchor = None
            continue
        seg_return = nav_pre / anchor - 1.0
        chain *= (1.0 + seg_return)
        anchor = after(navs, flow_ts)  # post-flow NAV opens the next epoch
        epochs.append((flow_ts, _Epoch(closed_chain=chain, anchor=anchor)))

    return epochs


def cumulative_twr(
    navs: pl.DataFrame,
    flows: list[int],
    start: int,
    grid: list[int],
) -> list[tuple[int, float | None]]:
    """Cumulative TWR equity curve at each grid timestamp, rebased at *start*.

    Computes sub-period checkpoints once (one per flow event), then evaluates
    each grid point as a pure lookup into that checkpoint table — no mutable
    index state, no in-loop branching on flow position.

    At each grid timestamp *t*:
        1. Identify the active epoch (latest epoch whose ``epoch_start <= t``).
        2. Partial return for the open sub-period: ``nav_at_t / anchor - 1``.
        3. Report: ``closed_chain * (1 + partial) - 1``.

    Returns ``None`` when the epoch anchor is missing/non-positive or when
    the at-or-before NAV at the grid point is missing.
    """
    epochs = _build_epochs(navs, flows, start)
    epoch_starts = [ts for ts, _ in epochs]

    def _value_at(ts: int) -> float | None:
        idx = bisect.bisect_right(epoch_starts, ts) - 1
        if idx < 0:
            return None
        _, epoch = epochs[idx]
        if epoch.anchor is None or epoch.anchor <= 0:
            return None
        nav_at_ts = asof(navs, ts)
        if nav_at_ts is None:
            return None
        partial = nav_at_ts / epoch.anchor - 1.0
        return epoch.closed_chain * (1.0 + partial) - 1.0

    return [(ts, _value_at(ts)) for ts in grid]


# ---------------------------------------------------------------------------
# Benchmark helpers
# ---------------------------------------------------------------------------

def sample_at_grid(navs: pl.DataFrame, grid: list[int]) -> list[float | None]:
    """Benchmark price at each grid tick using asof (last value at-or-before).

    Mirrors numbat ``sample_at_grid``: forward-fill / step-function hold.
    Returns None for grid ticks where no price yet exists.
    """
    return [asof(navs, ts) for ts in grid]


def cum_returns_from_navs(values: list[float | None]) -> list[float | None]:
    """Simple cumulative return rebased at the first value: v_i / v_0 - 1.

    Returns None for every element when the first value is None or zero,
    and None for any element where the value itself is None.
    Mirrors numbat ``cum_returns_from_navs``.
    """
    if not values:
        return []
    anchor = values[0]
    if anchor is None or anchor == 0.0:
        return [None] * len(values)
    return [(v / anchor - 1.0) if v is not None else None for v in values]
