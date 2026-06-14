"""Risk-adjusted return statistics over a list of per-period returns.

Conventions (match numbat stdlib/stats.numbat):
  - Sample variance (n-1 denominator) for volatility and Sharpe.
  - Population-style denominator (n) for downside deviation in Sortino
    (Sortino 1991 original definition).
  - Annualisation basis: py = 365 / step_days  (e.g. 365 for daily, 8760 for hourly).
  - rf_per_period: de-annualise the annual risk-free to a per-period rate via
      (1 + rf_ann)^(1/py) - 1.

Public API
----------
annualize_return(total, n_periods, py)           — geometric rescaling
volatility_annualized(returns, py)               — sample stdev * sqrt(py)
sharpe(returns, rf_ann, py)                      — Sharpe ratio
sortino(returns, rf_ann, py)                     — Sortino ratio
calmar(ann_ret, max_dd)                          — ann_ret / abs(max_dd)
"""

from __future__ import annotations

import math


# ---------------------------------------------------------------------------
# Annualised return
# ---------------------------------------------------------------------------

def annualize_return(
    total: float | None,
    n_periods: int,
    py: float,
) -> float | None:
    """Geometrically rescale total return to annualised units.

    ann = (1 + total)^(py / n_periods) - 1.
    Returns None when total is None, 0 when n_periods <= 0 or total <= -1.
    """
    if total is None:
        return None
    if n_periods <= 0:
        return 0.0
    if total <= -1.0:
        return 0.0
    return (1.0 + total) ** (py / n_periods) - 1.0


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------

def _valid(returns: list[float | None]) -> list[float]:
    return [r for r in returns if r is not None]


def _sample_stdev(vals: list[float]) -> float:
    n = len(vals)
    if n < 2:
        return 0.0
    mean = sum(vals) / n
    variance = sum((x - mean) ** 2 for x in vals) / (n - 1)
    return math.sqrt(variance)


def _rf_per_period(rf_ann: float, py: float) -> float:
    return (1.0 + rf_ann) ** (1.0 / py) - 1.0


# ---------------------------------------------------------------------------
# Volatility
# ---------------------------------------------------------------------------

def volatility_annualized(
    returns: list[float | None],
    py: float,
) -> float | None:
    """Annualised volatility: sample stdev of period returns * sqrt(py).

    Returns None when fewer than 2 valid returns are available.
    """
    vals = _valid(returns)
    if len(vals) < 2:
        return None
    return _sample_stdev(vals) * math.sqrt(py)


# ---------------------------------------------------------------------------
# Sharpe
# ---------------------------------------------------------------------------

def sharpe(
    returns: list[float | None],
    rf_ann: float,
    py: float,
) -> float | None:
    """Annualised Sharpe ratio.

    (mean(r) - rf_per_period) / sample_stdev(r) * sqrt(py).
    Returns None when fewer than 2 valid returns; 0 when stdev == 0.
    """
    vals = _valid(returns)
    if len(vals) < 2:
        return None
    sd = _sample_stdev(vals)
    if sd == 0.0:
        return 0.0
    rf_p = _rf_per_period(rf_ann, py)
    mean_r = sum(vals) / len(vals)
    return (mean_r - rf_p) / sd * math.sqrt(py)


# ---------------------------------------------------------------------------
# Sortino
# ---------------------------------------------------------------------------

def sortino(
    returns: list[float | None],
    rf_ann: float,
    py: float,
) -> float | None:
    """Annualised Sortino ratio.

    Numerator same as Sharpe.  Denominator = downside deviation with
    population (n) denominator — Sortino 1991 original definition:
      dd = sqrt(mean(min(r - rf_p, 0)^2)).
    Returns None when fewer than 2 valid returns; 0 when dd == 0.
    """
    vals = _valid(returns)
    if len(vals) < 2:
        return None
    rf_p = _rf_per_period(rf_ann, py)
    mean_r = sum(vals) / len(vals)
    neg_sq_sum = sum((min(r - rf_p, 0.0)) ** 2 for r in vals)
    dd = math.sqrt(neg_sq_sum / len(vals))
    if dd == 0.0:
        return 0.0
    return (mean_r - rf_p) / dd * math.sqrt(py)


# ---------------------------------------------------------------------------
# Calmar
# ---------------------------------------------------------------------------

def calmar(ann_ret: float | None, max_dd: float | None) -> float | None:
    """Calmar ratio: ann_ret / abs(max_dd).

    Returns None when either input is None.
    Returns 0 when max_dd >= 0 (no drawdown or zero denominator).
    """
    if ann_ret is None or max_dd is None:
        return None
    if max_dd >= 0.0:
        return 0.0
    return ann_ret / abs(max_dd)
