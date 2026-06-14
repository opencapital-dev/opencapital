"""Time-Weighted Return over a polars NAV DataFrame.

NAV DataFrame columns
---------------------
ts    : Int64   — microsecond epoch timestamp (ascending, no duplicates required)
value : Float64 — portfolio value; null entries are skipped by all lookups

Public API
----------
asof(navs, t)            — last non-null NAV at or before t
before(navs, t)          — last non-null NAV strictly before t
after(navs, t)           — first non-null NAV at or after t
bounds(flows, t0, t1)    — sub-period boundary list
seg(navs, a, b, first, last) — one sub-period fractional return
twr(navs, flows, t0, t1) — time-weighted return scalar, or None
"""

from __future__ import annotations

from itertools import pairwise
from math import prod

import polars as pl


def asof(navs: pl.DataFrame, t: int) -> float | None:
    """Last non-null NAV at or before *t* (microseconds).

    Walks backward from the last row with ts <= t until a non-null value is found.
    Returns None when no such row exists.
    """
    candidates = navs.filter(pl.col("ts") <= t).drop_nulls("value")
    if candidates.is_empty():
        return None
    return candidates[-1]["value"][0]


def before(navs: pl.DataFrame, t: int) -> float | None:
    """Last non-null NAV strictly before *t* (microseconds).

    Returns None when no such row exists.
    """
    candidates = navs.filter(pl.col("ts") < t).drop_nulls("value")
    if candidates.is_empty():
        return None
    return candidates[-1]["value"][0]


def after(navs: pl.DataFrame, t: int) -> float | None:
    """First non-null NAV at or after *t* (microseconds).

    Returns None when no such row exists.
    """
    candidates = navs.filter(pl.col("ts") >= t).drop_nulls("value")
    if candidates.is_empty():
        return None
    return candidates[0]["value"][0]


def bounds(flows: list[int], t0: int, t1: int) -> list[int]:
    """Sub-period boundary timestamps for the window [t0, t1].

    Flows strictly inside the window are inserted in ascending order between
    t0 and t1, producing the standard GIPS boundary list.
    """
    return [t0, *sorted(f for f in flows if t0 < f < t1), t1]


def seg(
    navs: pl.DataFrame,
    a: int,
    b: int,
    first: bool,
    last: bool,
) -> float | None:
    """Fractional return for one sub-period [a, b].

    Open NAV (nav_a):
        first segment  → at-or-before a  (opening mark is the window anchor)
        other segments → at-or-after  a  (post-flow NAV opens the next segment)

    Close NAV (nav_b):
        last segment   → at-or-before b  (closing mark is the window anchor)
        other segments → strictly-before b  (pre-flow NAV closes the segment)

    Returns None if either anchor is missing or nav_a <= 0.
    """
    nav_a = asof(navs, a) if first else after(navs, a)
    nav_b = asof(navs, b) if last else before(navs, b)
    if nav_a is None or nav_b is None or nav_a <= 0:
        return None
    return nav_b / nav_a - 1.0


def twr(
    navs: pl.DataFrame,
    flows: list[int],
    t0: int,
    t1: int,
) -> float | None:
    """Time-weighted return over the window [t0, t1].

    Implements the GIPS sub-period chain formula:
        TWR = prod(1 + r_i) - 1

    where each r_i is the return of one sub-period separated by external flows.

    Returns None when:
    - t1 <= t0 (degenerate window)
    - any sub-period anchor NAV is missing or nav_a <= 0
    """
    if t1 <= t0:
        return None
    segs = list(pairwise(bounds(flows, t0, t1)))
    last = len(segs) - 1
    factors: list[float] = []
    for i, (a, b) in enumerate(segs):
        r = seg(navs, a, b, i == 0, i == last)
        if r is None:
            return None
        factors.append(1.0 + r)
    return prod(factors) - 1.0
