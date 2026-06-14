"""Rolling and expanding linear-regression statistics for a return pair.

Given two aligned per-period return series ``r_p`` (portfolio) and
``r_b`` (benchmark), compute the CAPM-style fit at every grid index:

    beta = cov(r_p, r_b) / var(r_b)
    alpha = mean(r_p) - beta * mean(r_b)
    corr = cov / (sd_p * sd_b)
    r_squared = corr ** 2

A "valid pair" means both ``r_p[i]`` and ``r_b[i]`` are non-None.
"""

from __future__ import annotations

import math
from collections import deque
from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class RegressionStats:
    """OLS fit over the sample of valid ``(r_p, r_b)`` pairs in scope."""

    beta: float | None
    alpha: float | None
    corr: float | None
    r_squared: float | None
    var_p: float | None
    var_b: float | None
    cov: float | None
    n_obs: int


def cumulative_to_period_returns(
    cumulative: list[tuple[int, float | None]],
) -> list[tuple[int, float | None]]:
    """Convert a cumulative-return grid to per-period returns."""
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


def rolling_regression_stats(
    *,
    r_p: list[tuple[int, float | None]],
    r_b: list[tuple[int, float | None]],
    lookback: int,
) -> list[RegressionStats | None]:
    """Per-index regression stats over the trailing ``lookback`` valid pairs."""
    if lookback < 2:
        raise ValueError(f"lookback must be >= 2, got {lookback}")
    _require_aligned(r_p, r_b)
    window: deque[tuple[float, float]] = deque()
    s_p = s_b = s_pp = s_bb = s_pb = 0.0
    out: list[RegressionStats | None] = []
    for i in range(len(r_p)):
        p_val = r_p[i][1]
        b_val = r_b[i][1]
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


def expanding_regression_stats(
    *,
    r_p: list[tuple[int, float | None]],
    r_b: list[tuple[int, float | None]],
    min_obs: int = 2,
) -> list[RegressionStats | None]:
    """Per-index regression stats over every valid pair up to ``i``."""
    if min_obs < 2:
        raise ValueError(f"min_obs must be >= 2, got {min_obs}")
    _require_aligned(r_p, r_b)
    s_p = s_b = s_pp = s_bb = s_pb = 0.0
    n = 0
    out: list[RegressionStats | None] = []
    for i in range(len(r_p)):
        p_val = r_p[i][1]
        b_val = r_b[i][1]
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


def _stats_from_sums(
    s_p: float, s_b: float,
    s_pp: float, s_bb: float, s_pb: float,
    n: int,
) -> RegressionStats | None:
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


def _require_aligned(
    a: list[tuple[int, float | None]],
    b: list[tuple[int, float | None]],
) -> None:
    if len(a) != len(b):
        raise ValueError(
            f"r_p / r_b length mismatch: {len(a)} vs {len(b)}"
        )
    for i, ((t_a, _), (t_b, _)) in enumerate(zip(a, b, strict=True)):
        if t_a != t_b:
            raise ValueError(
                f"r_p / r_b timestamp misalignment at index {i}: "
                f"{t_a} vs {t_b}"
            )
