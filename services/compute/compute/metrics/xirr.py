"""XIRR — Extended Internal Rate of Return for irregular cashflows.

Matches the numbat reference (the metric reference +
the metric reference) exactly:

  Day-count basis: ACT/365.25  (xirr_year = 365.25 * 86 400 s = 365.25 * 86 400 * 1 000 000 µs)
  Time origin:     t0 = timestamp of the first cashflow in the list.
  Sign convention: investor-out (deposit) = negative;
                   investor-in (receipt / terminal NAV) = positive.
  Solver:          pure bisection on bracket [-0.99, 10.0], 60 halvings
                   (~1e-18 precision); returns None when the bracket has no
                   sign change (all-same-sign cashflows or < 2 flows).

The annualized cumulative variant follows xirr_cumulative.numbat:
  cumulative = (1 + xirr_annualized)^window_nyears - 1
  window_nyears = (window_end - window_start) / 365.25 days.

Public API
----------
xirr(flows)                                           → float | None
xirr_cumulative(flows, *, window_start, window_end)   → float | None
npv_at(rate, flows)                                   → float
"""

from __future__ import annotations

_XIRR_YEAR_US: int = int(365.25 * 86_400 * 1_000_000)

# Bisection bracket and iteration count mirror stdlib/xirr.numbat exactly.
_LO: float = -0.99
_HI: float = 10.0
_ITERS: int = 60


def npv_at(rate: float, flows: list[tuple[int, float]]) -> float:
    """Net present value at ``rate`` for ``flows``, ACT/365.25 basis.

    NPV(r) = sum( amt_i / (1 + r)^((ts_i − t0) / xirr_year) )

    ``flows`` is a list of ``(ts_us, amount)`` pairs where ``ts_us`` is a
    microsecond-epoch int timestamp.  t0 is the timestamp of the first flow.
    Raises ValueError when ``flows`` is empty.
    """
    if not flows:
        raise ValueError("flows must not be empty")
    t0_us = flows[0][0]
    total = 0.0
    for ts_us, amt in flows:
        t_years = (ts_us - t0_us) / _XIRR_YEAR_US
        total += amt / (1.0 + rate) ** t_years
    return total


def xirr(flows: list[tuple[int, float]]) -> float | None:
    """Annualized XIRR for a sequence of irregular cashflows.

    ``flows`` is a list of ``(ts_us, signed_amount)`` pairs where ``ts_us`` is
    a microsecond-epoch int timestamp.  Pairs may be in any order (sorted by
    timestamp internally).  Sign convention: deposits are negative (money
    leaving the investor), receipts are positive.

    Returns the rate ``r`` such that NPV(r, flows) = 0, computed via pure
    bisection on [-0.99, 10.0] with 60 halvings (≈1e-18 precision on the
    rate, well below 1e-9 tolerance).

    Returns ``None`` — never NaN or inf — when:
      * fewer than 2 flows are provided, or
      * the bracket [-0.99, 10.0] has no sign change (all flows the same sign,
        or the NPV is never zero inside the bracket).
    """
    if len(flows) < 2:
        return None

    # Sort chronologically so t0 is genuinely the earliest timestamp.
    sorted_flows = sorted(flows, key=lambda f: f[0])

    def residual(rate: float) -> float:
        return npv_at(rate, sorted_flows)

    lo_val = residual(_LO)
    hi_val = residual(_HI)

    # No sign change → no root in bracket → return None.
    if lo_val * hi_val >= 0.0:
        return None

    lo, hi = _LO, _HI
    for _ in range(_ITERS):
        mid = (lo + hi) / 2.0
        mid_val = residual(mid)
        if lo_val * mid_val < 0.0:
            hi = mid
        else:
            lo = mid
            lo_val = mid_val

    return (lo + hi) / 2.0


def xirr_cumulative(
    flows: list[tuple[int, float]],
    *,
    window_start: int,
    window_end: int,
) -> float | None:
    """Cumulative return equivalent of the annualized XIRR over a window.

    Implements the formula from xirr_cumulative.numbat:

        cumulative = (1 + xirr_annualized)^window_nyears - 1
        window_nyears = (window_end - window_start) / xirr_year

    ``window_start`` and ``window_end`` are microsecond-epoch int timestamps.
    Returns ``None`` when xirr(flows) is None.
    """
    ann = xirr(flows)
    if ann is None:
        return None
    nyears = (window_end - window_start) / _XIRR_YEAR_US
    return (1.0 + ann) ** nyears - 1.0
