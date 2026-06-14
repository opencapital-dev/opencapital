"""Rolling and expanding linear-regression statistics over polars return DataFrames.

Polars convention (matching T2/T3):
    DataFrame with columns  ts: Int64, ret: Float64  (nulls for missing periods)

The formulas read directly as the definitions:

    beta      = cov(r_p, r_b) / var(r_b)
    alpha     = mean(r_p) - beta * mean(r_b)
    corr      = cov / (sd_p * sd_b)
    r_squared = corr**2

Variance is population variance (ddof=0), consistent with the reference
accumulator:  var = E[x^2] - E[x]^2.

Alignment: both DataFrames must share the same ``ts`` column in the same order.
Null in either ``ret`` causes that pair to be skipped (not counted as an
observation), matching the reference's None-skipping behaviour.

Public API
----------
cumulative_to_period_returns(cumulative)         — cumulative → per-period returns
rolling_regression_stats(r_p, r_b, lookback)    — trailing-lookback OLS per index
expanding_regression_stats(r_p, r_b, min_obs)   — expanding-window OLS per index
"""

from __future__ import annotations

import math
from collections import deque
from dataclasses import dataclass

import polars as pl


@dataclass(frozen=True, slots=True)
class RegressionStats:
    """OLS fit over the sample of valid ``(r_p, r_b)`` pairs in scope.

    Fields
    ------
    beta       = cov(r_p, r_b) / var(r_b)
    alpha      = mean(r_p) - beta * mean(r_b)
    corr       = cov / (sd_p * sd_b)
    r_squared  = corr**2
    var_p      — population variance of r_p over the window
    var_b      — population variance of r_b over the window
    cov        — population covariance of (r_p, r_b) over the window
    n_obs      — number of valid (non-null) pairs included

    beta/alpha/corr/r_squared are None when var_b == 0 (undefined regression).
    """

    beta: float | None
    alpha: float | None
    corr: float | None
    r_squared: float | None
    var_p: float | None
    var_b: float | None
    cov: float | None
    n_obs: int


# ---------------------------------------------------------------------------
# Helpers shared with the reference (same pure-Python logic, no common/ call)
# ---------------------------------------------------------------------------

def cumulative_to_period_returns(
    cumulative: list[tuple[int, float | None]],
) -> list[tuple[int, float | None]]:
    """Convert a (ts, cum) grid to per-period returns.

    Period return at index i is (1 + cum_i) / (1 + cum_{i-1}) - 1.
    The first element is always (ts, None) because there is no prior period.
    None propagates when either value is missing or the prior cumulative equals -1.
    """
    if not cumulative:
        return []
    out: list[tuple[int, float | None]] = [(cumulative[0][0], None)]
    for i in range(1, len(cumulative)):
        ts = cumulative[i][0]
        c_now = cumulative[i][1]
        c_prev = cumulative[i - 1][1]
        if c_now is None or c_prev is None:
            out.append((ts, None))
            continue
        denom = 1.0 + c_prev
        if denom == 0.0:
            out.append((ts, None))
            continue
        out.append((ts, (1.0 + c_now) / denom - 1.0))
    return out


def cum_to_period_returns(cum: list[float | None]) -> list[float | None]:
    """Per-period returns from a flat cumulative-return list.

    r_i = (1 + cum_i) / (1 + cum_{i-1}) - 1.
    First element is None (no prior period).  None propagates when either
    neighbour is None or the prior denominator equals -1.
    """
    if not cum:
        return []
    out: list[float | None] = [None]
    for i in range(1, len(cum)):
        c_now = cum[i]
        c_prev = cum[i - 1]
        if c_now is None or c_prev is None:
            out.append(None)
            continue
        denom = 1.0 + c_prev
        if denom == 0.0:
            out.append(None)
            continue
        out.append((1.0 + c_now) / denom - 1.0)
    return out


# ---------------------------------------------------------------------------
# Internal: OLS from running sum-of-squares accumulators
# ---------------------------------------------------------------------------

def _stats_from_sums(
    s_p: float, s_b: float,
    s_pp: float, s_bb: float, s_pb: float,
    n: int,
) -> RegressionStats | None:
    """Compute OLS stats from sum accumulators over n valid pairs.

    Uses population variance (ddof=0):
        var = E[x^2] - E[x]^2  =  s_xx/n - (s_x/n)^2
    Returns None when n < 2.
    """
    if n < 2:
        return None
    mean_p = s_p / n
    mean_b = s_b / n
    var_p = s_pp / n - mean_p * mean_p
    var_b = s_bb / n - mean_b * mean_b
    cov = s_pb / n - mean_p * mean_b
    if var_p < 0.0:
        var_p = 0.0
    if var_b < 0.0:
        var_b = 0.0
    if var_b <= 0.0:
        return RegressionStats(
            beta=None, alpha=None, corr=None, r_squared=None,
            var_p=var_p, var_b=var_b, cov=cov, n_obs=n,
        )
    beta = cov / var_b
    alpha = mean_p - beta * mean_b
    sd_p = math.sqrt(var_p)
    sd_b = math.sqrt(var_b)
    if sd_p == 0.0:
        corr = 0.0
        r_squared = 0.0
    else:
        corr = cov / (sd_p * sd_b)
        corr = max(-1.0, min(1.0, corr))
        r_squared = corr * corr
    return RegressionStats(
        beta=beta, alpha=alpha, corr=corr, r_squared=r_squared,
        var_p=var_p, var_b=var_b, cov=cov, n_obs=n,
    )


# ---------------------------------------------------------------------------
# Alignment validation
# ---------------------------------------------------------------------------

def _require_aligned(r_p: pl.DataFrame, r_b: pl.DataFrame) -> None:
    ts_p = r_p["ts"].to_list()
    ts_b = r_b["ts"].to_list()
    if len(ts_p) != len(ts_b):
        raise ValueError(f"r_p / r_b length mismatch: {len(ts_p)} vs {len(ts_b)}")
    for i, (t_a, t_b) in enumerate(zip(ts_p, ts_b)):
        if t_a != t_b:
            raise ValueError(
                f"r_p / r_b timestamp misalignment at index {i}: {t_a} vs {t_b}"
            )


# ---------------------------------------------------------------------------
# Public: rolling OLS
# ---------------------------------------------------------------------------

def rolling_regression_stats(
    *,
    r_p: pl.DataFrame,
    r_b: pl.DataFrame,
    lookback: int,
) -> list[RegressionStats | None]:
    """Per-index regression stats over the trailing ``lookback`` valid pairs.

    Each index emits stats once exactly ``lookback`` valid (non-null) pairs
    fill the sliding window; otherwise emits None.

    ``r_p`` and ``r_b`` must be DataFrames with columns ts: Int64, ret: Float64.
    Both must have identical ts columns in identical order.
    """
    if lookback < 2:
        raise ValueError(f"lookback must be >= 2, got {lookback}")
    _require_aligned(r_p, r_b)
    p_vals = r_p["ret"].to_list()
    b_vals = r_b["ret"].to_list()
    window: deque[tuple[float, float]] = deque()
    s_p = s_b = s_pp = s_bb = s_pb = 0.0
    out: list[RegressionStats | None] = []
    for p_val, b_val in zip(p_vals, b_vals):
        if p_val is not None and b_val is not None:
            if len(window) >= lookback:
                old_p, old_b = window.popleft()
                s_p -= old_p
                s_b -= old_b
                s_pp -= old_p * old_p
                s_bb -= old_b * old_b
                s_pb -= old_p * old_b
            window.append((p_val, b_val))
            s_p += p_val
            s_b += b_val
            s_pp += p_val * p_val
            s_bb += b_val * b_val
            s_pb += p_val * b_val
        out.append(
            _stats_from_sums(s_p, s_b, s_pp, s_bb, s_pb, len(window))
            if len(window) == lookback
            else None
        )
    return out


# ---------------------------------------------------------------------------
# Public: expanding OLS
# ---------------------------------------------------------------------------

def expanding_regression_stats(
    *,
    r_p: pl.DataFrame,
    r_b: pl.DataFrame,
    min_obs: int = 2,
) -> list[RegressionStats | None]:
    """Per-index regression stats over every valid pair up to index ``i``.

    Emits None until ``min_obs`` valid pairs have been accumulated.

    ``r_p`` and ``r_b`` must be DataFrames with columns ts: Int64, ret: Float64.
    Both must have identical ts columns in identical order.
    """
    if min_obs < 2:
        raise ValueError(f"min_obs must be >= 2, got {min_obs}")
    _require_aligned(r_p, r_b)
    p_vals = r_p["ret"].to_list()
    b_vals = r_b["ret"].to_list()
    s_p = s_b = s_pp = s_bb = s_pb = 0.0
    n = 0
    out: list[RegressionStats | None] = []
    for p_val, b_val in zip(p_vals, b_vals):
        if p_val is not None and b_val is not None:
            n += 1
            s_p += p_val
            s_b += b_val
            s_pp += p_val * p_val
            s_bb += b_val * b_val
            s_pb += p_val * b_val
        out.append(
            _stats_from_sums(s_p, s_b, s_pp, s_bb, s_pb, n)
            if n >= min_obs
            else None
        )
    return out
