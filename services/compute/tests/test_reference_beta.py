"""Beta helper tests -- rolling + expanding OLS over return pairs."""

from __future__ import annotations

import pytest

from tests.reference.beta import (
    cumulative_to_period_returns,
    expanding_regression_stats,
    rolling_regression_stats,
)


def _series(values: list[float | None]) -> list[tuple[int, float | None]]:
    return [(i, v) for i, v in enumerate(values)]


def test_cumulative_to_period_returns_basic():
    cum = _series([0.0, 0.10, 0.21])
    period = cumulative_to_period_returns(cum)
    assert period[0] == (0, None)
    assert period[1][1] == pytest.approx(0.10)
    assert period[2][1] == pytest.approx(0.10)


def test_cumulative_to_period_returns_propagates_none():
    cum = _series([0.0, None, 0.20])
    period = cumulative_to_period_returns(cum)
    assert period[1][1] is None
    assert period[2][1] is None


def test_rolling_perfect_correlation_gives_beta_one():
    rets = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0, 0.015, -0.005, 0.01]
    series = _series([None] + rets)
    out = rolling_regression_stats(r_p=series, r_b=series, lookback=5)
    last = out[-1]
    assert last is not None
    assert last.beta == pytest.approx(1.0)
    assert last.alpha == pytest.approx(0.0, abs=1e-12)
    assert last.corr == pytest.approx(1.0)
    assert last.r_squared == pytest.approx(1.0)
    assert last.n_obs == 5


def test_rolling_anti_correlation_gives_beta_minus_one():
    r_b_vals = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0, 0.015]
    r_p_vals = [-v for v in r_b_vals]
    r_b = _series([None] + r_b_vals)
    r_p = _series([None] + r_p_vals)
    out = rolling_regression_stats(r_p=r_p, r_b=r_b, lookback=5)
    last = out[-1]
    assert last is not None
    assert last.beta == pytest.approx(-1.0)
    assert last.corr == pytest.approx(-1.0)
    assert last.r_squared == pytest.approx(1.0)


def test_rolling_lookback_not_yet_filled_emits_none():
    rets = [0.01, 0.02, 0.03]
    series = _series([None] + rets)
    out = rolling_regression_stats(r_p=series, r_b=series, lookback=5)
    assert all(stats is None for stats in out)


def test_rolling_zero_variance_benchmark_gives_none_beta():
    r_p_vals = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02]
    r_b_vals = [0.0] * len(r_p_vals)
    out = rolling_regression_stats(
        r_p=_series([None] + r_p_vals),
        r_b=_series([None] + r_b_vals),
        lookback=4,
    )
    last = out[-1]
    assert last is not None
    assert last.beta is None
    assert last.alpha is None
    assert last.corr is None
    assert last.r_squared is None
    assert last.var_b == pytest.approx(0.0, abs=1e-18)
    assert last.n_obs == 4


def test_expanding_matches_rolling_when_lookback_covers_all():
    rets_p = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0]
    rets_b = [0.012, -0.018, 0.028, 0.006, -0.012, 0.024, 0.002]
    r_p = _series([None] + rets_p)
    r_b = _series([None] + rets_b)
    n_valid = len(rets_p)
    rolling = rolling_regression_stats(r_p=r_p, r_b=r_b, lookback=n_valid)
    expanding = expanding_regression_stats(r_p=r_p, r_b=r_b)
    assert rolling[-1] is not None
    assert expanding[-1] is not None
    assert rolling[-1].beta == pytest.approx(expanding[-1].beta)
    assert rolling[-1].alpha == pytest.approx(expanding[-1].alpha)
    assert rolling[-1].corr == pytest.approx(expanding[-1].corr)


def test_expanding_known_beta():
    rng_r_b = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0, 0.015, -0.005, 0.01]
    rng_r_p = [1.5 * v + 0.001 for v in rng_r_b]
    out = expanding_regression_stats(
        r_p=_series([None] + rng_r_p),
        r_b=_series([None] + rng_r_b),
    )
    last = out[-1]
    assert last is not None
    assert last.beta == pytest.approx(1.5)
    assert last.alpha == pytest.approx(0.001)
    assert last.r_squared == pytest.approx(1.0)


def test_expanding_skips_none_pairs():
    r_p = _series([None, 0.01, None, 0.03, 0.005, None, 0.02])
    r_b = _series([None, 0.02, 0.01, 0.06, 0.01, 0.03, 0.04])
    out = expanding_regression_stats(r_p=r_p, r_b=r_b)
    last = out[-1]
    assert last is not None
    assert last.n_obs == 4


def test_misaligned_timestamps_raise():
    a = [(0, 0.0), (1, 0.0)]
    b = [(0, 0.0), (2, 0.0)]
    with pytest.raises(ValueError, match="timestamp misalignment"):
        rolling_regression_stats(r_p=a, r_b=b, lookback=2)


def test_lookback_less_than_two_raises():
    series = _series([None, 0.01, 0.02])
    with pytest.raises(ValueError, match="lookback"):
        rolling_regression_stats(r_p=series, r_b=series, lookback=1)
