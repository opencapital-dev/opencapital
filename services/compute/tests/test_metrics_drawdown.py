"""Unit tests for compute.metrics.drawdown and compute.metrics.riskstats.

Parity oracle: hand-computed values on small known series + comparison
with numbat stdlib formulas.

Drawdown sign convention (matches numbat stats.numbat):
  - max_drawdown returns the most negative ratio, e.g. -0.20 for a 20% peak-to-trough drop.
  - current_drawdown returns 0 when the last point is at or above the running peak.

Sortino denominator: population (n) — matches numbat stats.numbat 'downside_deviation'.
Sample stdev: (n-1) denominator — matches numbat stats.numbat 'sample_stdev'.
"""

from __future__ import annotations

import math

import pytest

from compute.metrics.beta import cum_to_period_returns
from compute.metrics.drawdown import max_drawdown, current_drawdown, drawdown_episodes
from compute.metrics.riskstats import (
    annualize_return,
    volatility_annualized,
    sharpe,
    sortino,
    calmar,
)

_D = 86_400_000_000  # 1 day in µs


# ---------------------------------------------------------------------------
# max_drawdown
# ---------------------------------------------------------------------------

def test_max_drawdown_empty():
    assert max_drawdown([]) is None


def test_max_drawdown_all_none():
    assert max_drawdown([None, None]) is None


def test_max_drawdown_no_drop():
    # Always rising — no drawdown
    cum = [0.0, 0.05, 0.10, 0.20]
    assert max_drawdown(cum) == pytest.approx(0.0)


def test_max_drawdown_single_drop():
    # Peak at +10%, drops to -5% from anchor → eq goes 1.10 → 0.95.
    # dd = 0.95 / 1.10 - 1 = -0.136363...
    cum = [0.0, 0.10, -0.05]
    expected = (1.0 - 0.05) / (1.0 + 0.10) - 1.0
    assert max_drawdown(cum) == pytest.approx(expected, rel=1e-9)


def test_max_drawdown_recovers_then_drops_again():
    # eq: 1.0, 1.1, 1.05, 1.2, 1.0
    # After recovery from first drop, second drop: 1.0/1.2 - 1 = -0.1667
    cum = [0.0, 0.1, 0.05, 0.2, 0.0]
    # Walk: peak starts 1.0
    # 1.1 >= 1.0 → peak=1.1
    # 1.05 < 1.1 → dd = 1.05/1.1 - 1 = -0.04545
    # 1.2 >= 1.1 → peak=1.2
    # 1.0 < 1.2  → dd = 1.0/1.2 - 1 = -0.1667  ← min
    expected = 1.0 / 1.2 - 1.0
    assert max_drawdown(cum) == pytest.approx(expected, rel=1e-9)


def test_max_drawdown_single_point():
    assert max_drawdown([0.05]) == pytest.approx(0.0)


def test_max_drawdown_with_none_gaps():
    # None values are skipped; max_dd uses only valid points
    cum = [0.0, None, 0.10, -0.05]
    expected = (1.0 - 0.05) / (1.0 + 0.10) - 1.0
    assert max_drawdown(cum) == pytest.approx(expected, rel=1e-9)


# ---------------------------------------------------------------------------
# current_drawdown
# ---------------------------------------------------------------------------

def test_current_drawdown_empty():
    assert current_drawdown([]) is None


def test_current_drawdown_at_peak():
    cum = [0.0, 0.05, 0.10]
    assert current_drawdown(cum) == pytest.approx(0.0)


def test_current_drawdown_below_peak():
    # eq: 1.0, 1.2, 1.0  → peak=1.2, last=1.0 → 1.0/1.2 - 1
    cum = [0.0, 0.2, 0.0]
    expected = 1.0 / 1.2 - 1.0
    assert current_drawdown(cum) == pytest.approx(expected, rel=1e-9)


def test_current_drawdown_recovered():
    # Drops then recovers — last point at peak → 0
    cum = [0.0, 0.2, -0.05, 0.3]
    assert current_drawdown(cum) == pytest.approx(0.0)


# ---------------------------------------------------------------------------
# drawdown_episodes
# ---------------------------------------------------------------------------

def test_drawdown_episodes_empty():
    assert drawdown_episodes([], []) == []


def test_drawdown_episodes_no_drawdown():
    cum = [0.0, 0.05, 0.10]
    grid = [0, _D, 2 * _D]
    eps = drawdown_episodes(cum, grid)
    assert eps == []


def test_drawdown_episodes_one_complete():
    # eq: 1.0, 1.2, 0.9, 1.3
    # peak=1.0 at t=0, rises to 1.2 at t=1 (peak=1.2)
    # drops to 0.9 at t=2 → episode opens, peak_ts=1d, trough=0.9 at 2d
    # 1.3 >= 1.2 at t=3 → episode closes, recovery_ts=3d
    cum = [0.0, 0.2, -0.1, 0.3]
    grid = [0, _D, 2 * _D, 3 * _D]
    eps = drawdown_episodes(cum, grid)
    assert len(eps) == 1
    ep = eps[0]
    assert ep.peak_ts == _D
    assert ep.trough_ts == 2 * _D
    assert ep.recovery_ts == 3 * _D
    assert ep.depth == pytest.approx(0.9 / 1.2 - 1.0, rel=1e-9)
    assert ep.time_to_trough_sec == pytest.approx(_D / 1_000_000)
    assert ep.time_to_recover_sec == pytest.approx(2 * _D / 1_000_000)


def test_drawdown_episodes_unrecovered():
    # eq: 1.0, 1.1, 0.8  → never recovers → recovery_ts = None
    cum = [0.0, 0.1, -0.2]
    grid = [0, _D, 2 * _D]
    eps = drawdown_episodes(cum, grid)
    assert len(eps) == 1
    ep = eps[0]
    assert ep.recovery_ts is None
    assert ep.time_to_recover_sec is None
    assert ep.depth == pytest.approx(0.8 / 1.1 - 1.0, rel=1e-9)


def test_drawdown_episodes_two_episodes():
    # Two complete drawdown cycles
    cum = [0.0, 0.1, -0.05, 0.15, 0.05, 0.2]
    grid = [i * _D for i in range(6)]
    eps = drawdown_episodes(cum, grid)
    assert len(eps) == 2


# ---------------------------------------------------------------------------
# cum_to_period_returns
# ---------------------------------------------------------------------------

def test_cum_to_period_returns_empty():
    assert cum_to_period_returns([]) == []


def test_cum_to_period_returns_single():
    result = cum_to_period_returns([0.05])
    assert result == [None]


def test_cum_to_period_returns_basic():
    # cum: 0.0, 0.05, 0.1
    # period[1] = (1.05) / (1.00) - 1 = 0.05
    # period[2] = (1.10) / (1.05) - 1 = 0.04761...
    cum = [0.0, 0.05, 0.10]
    result = cum_to_period_returns(cum)
    assert result[0] is None
    assert result[1] == pytest.approx(0.05, rel=1e-9)
    assert result[2] == pytest.approx(1.10 / 1.05 - 1.0, rel=1e-9)


def test_cum_to_period_returns_none_propagates():
    cum = [0.0, None, 0.10]
    result = cum_to_period_returns(cum)
    assert result[0] is None
    assert result[1] is None
    assert result[2] is None  # prev was None


# ---------------------------------------------------------------------------
# annualize_return
# ---------------------------------------------------------------------------

def test_annualize_return_none_total():
    assert annualize_return(None, 10, 365.0) is None


def test_annualize_return_zero_periods():
    assert annualize_return(0.10, 0, 365.0) == 0.0


def test_annualize_return_total_wipeout():
    assert annualize_return(-1.0, 10, 365.0) == 0.0


def test_annualize_return_one_year():
    # total=0.10 over 365 periods (daily), py=365 → ann = 1.10^(365/365) - 1 = 0.10
    assert annualize_return(0.10, 365, 365.0) == pytest.approx(0.10, rel=1e-9)


def test_annualize_return_half_year():
    # total=0.10 over 182 periods, py=365 → (1.10)^(365/182) - 1
    total, n, py = 0.10, 182, 365.0
    expected = (1.0 + total) ** (py / n) - 1.0
    assert annualize_return(total, n, py) == pytest.approx(expected, rel=1e-9)


# ---------------------------------------------------------------------------
# volatility_annualized
# ---------------------------------------------------------------------------

def test_volatility_none_when_too_few():
    assert volatility_annualized([], 365.0) is None
    assert volatility_annualized([0.01], 365.0) is None
    assert volatility_annualized([None, None], 365.0) is None


def test_volatility_known_value():
    # Two returns: 0.01, 0.03 → sample stdev = sqrt(((0.01-0.02)^2 + (0.03-0.02)^2) / 1)
    # = sqrt(0.0002) = 0.014142...
    # annualised at py=252: 0.014142 * sqrt(252) = 0.22434...
    returns = [0.01, 0.03]
    sd = math.sqrt(((0.01 - 0.02) ** 2 + (0.03 - 0.02) ** 2) / 1)
    expected = sd * math.sqrt(252)
    assert volatility_annualized(returns, 252.0) == pytest.approx(expected, rel=1e-9)


def test_volatility_skips_none():
    returns = [0.01, None, 0.03]
    assert volatility_annualized(returns, 252.0) == pytest.approx(
        volatility_annualized([0.01, 0.03], 252.0), rel=1e-9
    )


# ---------------------------------------------------------------------------
# sharpe
# ---------------------------------------------------------------------------

def test_sharpe_none_when_too_few():
    assert sharpe([], 0.0, 365.0) is None
    assert sharpe([0.01], 0.0, 365.0) is None


def test_sharpe_zero_stdev():
    # All returns identical → stdev=0 → sharpe=0
    returns = [0.01, 0.01, 0.01]
    assert sharpe(returns, 0.0, 365.0) == pytest.approx(0.0)


def test_sharpe_known_value():
    # Daily returns: 0.01, 0.02, 0.03; rf_ann=0.0; py=365
    # mean=0.02, sample_stdev=sqrt(((0.01-0.02)^2+(0.02-0.02)^2+(0.03-0.02)^2)/2)
    # = sqrt(0.0002/2) = sqrt(0.0001) = 0.01
    # rf_p = 0.0; sharpe = (0.02 - 0) / 0.01 * sqrt(365)
    returns = [0.01, 0.02, 0.03]
    mean_r = 0.02
    sd = math.sqrt(((0.01 - 0.02) ** 2 + (0.02 - 0.02) ** 2 + (0.03 - 0.02) ** 2) / 2)
    expected = mean_r / sd * math.sqrt(365)
    assert sharpe(returns, 0.0, 365.0) == pytest.approx(expected, rel=1e-9)


def test_sharpe_with_rf():
    returns = [0.01, 0.02, 0.03]
    rf_ann = 0.05
    py = 365.0
    rf_p = (1.0 + rf_ann) ** (1.0 / py) - 1.0
    vals = returns
    mean_r = sum(vals) / len(vals)
    sd = math.sqrt(sum((r - mean_r) ** 2 for r in vals) / (len(vals) - 1))
    expected = (mean_r - rf_p) / sd * math.sqrt(py)
    assert sharpe(returns, rf_ann, py) == pytest.approx(expected, rel=1e-9)


# ---------------------------------------------------------------------------
# sortino
# ---------------------------------------------------------------------------

def test_sortino_none_when_too_few():
    assert sortino([], 0.0, 365.0) is None
    assert sortino([0.01], 0.0, 365.0) is None


def test_sortino_zero_downside():
    # All returns above rf → no downside → sortino=0
    returns = [0.05, 0.06, 0.07]
    assert sortino(returns, 0.0, 365.0) == pytest.approx(0.0)


def test_sortino_known_value():
    # returns: -0.01, 0.02, -0.03; rf_ann=0.0; py=365
    # mean=-0.00667; rf_p=0; negative excess: -0.01, -0.03
    # dd = sqrt((0.01^2 + 0.03^2) / 3) = sqrt((0.0001+0.0009)/3) = sqrt(0.001/3)
    # sortino = (mean / dd) * sqrt(365)
    returns = [-0.01, 0.02, -0.03]
    rf_p = 0.0
    n = 3
    mean_r = sum(returns) / n
    neg_sq = sum((min(r - rf_p, 0.0)) ** 2 for r in returns)
    dd = math.sqrt(neg_sq / n)
    expected = (mean_r - rf_p) / dd * math.sqrt(365.0)
    assert sortino(returns, 0.0, 365.0) == pytest.approx(expected, rel=1e-9)


# ---------------------------------------------------------------------------
# calmar
# ---------------------------------------------------------------------------

def test_calmar_none_inputs():
    assert calmar(None, -0.2) is None
    assert calmar(0.1, None) is None


def test_calmar_no_drawdown():
    assert calmar(0.15, 0.0) == pytest.approx(0.0)
    assert calmar(0.15, 0.01) == pytest.approx(0.0)


def test_calmar_known_value():
    ann_ret = 0.20
    max_dd = -0.10
    expected = 0.20 / 0.10
    assert calmar(ann_ret, max_dd) == pytest.approx(expected, rel=1e-9)


def test_calmar_negative_return():
    ann_ret = -0.05
    max_dd = -0.20
    expected = -0.05 / 0.20
    assert calmar(ann_ret, max_dd) == pytest.approx(expected, rel=1e-9)
