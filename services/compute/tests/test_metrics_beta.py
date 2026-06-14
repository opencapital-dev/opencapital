"""Parity tests: compute.metrics.beta vs tests.reference.beta reference.

Each test feeds identical data to both sides and asserts that outputs agree
to floating-point tolerance.  The reference is the source of truth for values.

Polars convention (matching T2/T3):
    DataFrame with columns  ts: Int64, ret: Float64 (nulls for missing periods)

Alignment: timestamps must match row-for-row; a mismatch raises ValueError.
Variance: population (ddof=0), matching the reference's sum-of-squares accumulator.
"""

from __future__ import annotations

import pytest
import polars as pl

from tests.reference.beta import (
    cumulative_to_period_returns,
    rolling_regression_stats,
    expanding_regression_stats,
)
from compute.metrics.beta import (
    cumulative_to_period_returns as my_cum_to_period,
    rolling_regression_stats as my_rolling,
    expanding_regression_stats as my_expanding,
)


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

def _series(values: list[float | None]) -> list[tuple[int, float | None]]:
    return [(i, v) for i, v in enumerate(values)]


def _df(values: list[float | None], ts_offset: int = 0) -> pl.DataFrame:
    """Build a ts/ret DataFrame from a list of values (None → null)."""
    ts = [i + ts_offset for i in range(len(values))]
    return pl.DataFrame(
        {"ts": ts, "ret": values},
        schema={"ts": pl.Int64, "ret": pl.Float64},
    )


def _assert_stats_match(got, ref, idx: int = -1) -> None:
    label = f"[index {idx}]"
    if ref is None:
        assert got is None, f"{label} expected None, got {got}"
        return
    assert got is not None, f"{label} expected stats, got None"
    assert got.n_obs == ref.n_obs, f"{label} n_obs: got {got.n_obs}, ref {ref.n_obs}"
    if ref.beta is None:
        assert got.beta is None, f"{label} beta: expected None, got {got.beta}"
        assert got.alpha is None
        assert got.corr is None
        assert got.r_squared is None
    else:
        assert got.beta == pytest.approx(ref.beta, rel=1e-9, abs=1e-14), f"{label} beta"
        assert got.alpha == pytest.approx(ref.alpha, rel=1e-9, abs=1e-14), f"{label} alpha"
        assert got.corr == pytest.approx(ref.corr, rel=1e-9, abs=1e-14), f"{label} corr"
        assert got.r_squared == pytest.approx(ref.r_squared, rel=1e-9, abs=1e-14), f"{label} r_squared"
    if ref.var_b is not None:
        assert got.var_b == pytest.approx(ref.var_b, abs=1e-18), f"{label} var_b"
    if ref.var_p is not None:
        assert got.var_p == pytest.approx(ref.var_p, abs=1e-18), f"{label} var_p"
    if ref.cov is not None:
        assert got.cov == pytest.approx(ref.cov, abs=1e-14), f"{label} cov"


# ---------------------------------------------------------------------------
# cumulative_to_period_returns parity
# ---------------------------------------------------------------------------

def test_cum_to_period_basic():
    cum = _series([0.0, 0.10, 0.21])
    ref = cumulative_to_period_returns(cum)
    got = my_cum_to_period(cum)
    assert got == ref or (
        len(got) == len(ref)
        and all(
            (g[1] is None and r[1] is None) or
            (g[1] is not None and r[1] is not None and g[1] == pytest.approx(r[1]))
            for g, r in zip(got, ref)
        )
    )


def test_cum_to_period_propagates_none():
    cum = _series([0.0, None, 0.20])
    ref = cumulative_to_period_returns(cum)
    got = my_cum_to_period(cum)
    assert got[0][1] is None
    assert got[1][1] is None
    assert got[2][1] is None
    assert got[1][1] == ref[1][1]
    assert got[2][1] == ref[2][1]


def test_cum_to_period_zero_denom():
    cum = _series([0.0, -1.0, 0.10])
    ref = cumulative_to_period_returns(cum)
    got = my_cum_to_period(cum)
    assert got[2][1] is None
    assert got[2][1] == ref[2][1]


def test_cum_to_period_empty():
    ref = cumulative_to_period_returns([])
    got = my_cum_to_period([])
    assert got == ref == []


# ---------------------------------------------------------------------------
# rolling_regression_stats parity
# ---------------------------------------------------------------------------

def _run_rolling_parity(r_p_vals, r_b_vals, lookback, prefix_none=True):
    """Helper: run both reference and polars impl and assert full parity."""
    prefix = [None] if prefix_none else []
    r_p_list = _series(prefix + r_p_vals)
    r_b_list = _series(prefix + r_b_vals)
    r_p_df = _df(prefix + r_p_vals)
    r_b_df = _df(prefix + r_b_vals)
    ref = rolling_regression_stats(r_p=r_p_list, r_b=r_b_list, lookback=lookback)
    got = my_rolling(r_p=r_p_df, r_b=r_b_df, lookback=lookback)
    assert len(got) == len(ref)
    for i, (g, r) in enumerate(zip(got, ref)):
        _assert_stats_match(g, r, i)


def test_rolling_perfect_correlation_parity():
    rets = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0, 0.015, -0.005, 0.01]
    _run_rolling_parity(rets, rets, lookback=5)


def test_rolling_anti_correlation_parity():
    r_b_vals = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0, 0.015]
    r_p_vals = [-v for v in r_b_vals]
    _run_rolling_parity(r_p_vals, r_b_vals, lookback=5)


def test_rolling_lookback_not_yet_filled_all_none():
    rets = [0.01, 0.02, 0.03]
    _run_rolling_parity(rets, rets, lookback=5)


def test_rolling_zero_variance_benchmark():
    r_p_vals = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02]
    r_b_vals = [0.0] * len(r_p_vals)
    _run_rolling_parity(r_p_vals, r_b_vals, lookback=4)


def test_rolling_known_beta_parity():
    r_b_vals = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0, 0.015]
    r_p_vals = [1.5 * v + 0.001 for v in r_b_vals]
    _run_rolling_parity(r_p_vals, r_b_vals, lookback=5)


def test_rolling_lookback_less_than_two_raises():
    r_p_df = _df([None, 0.01, 0.02])
    r_b_df = _df([None, 0.01, 0.02])
    with pytest.raises(ValueError, match="lookback"):
        my_rolling(r_p=r_p_df, r_b=r_b_df, lookback=1)


def test_rolling_misaligned_timestamps_raise():
    # Different ts columns → misalignment error
    r_p_df = pl.DataFrame({"ts": [0, 1], "ret": [0.0, 0.0]}, schema={"ts": pl.Int64, "ret": pl.Float64})
    r_b_df = pl.DataFrame({"ts": [0, 2], "ret": [0.0, 0.0]}, schema={"ts": pl.Int64, "ret": pl.Float64})
    with pytest.raises(ValueError, match="timestamp misalignment"):
        my_rolling(r_p=r_p_df, r_b=r_b_df, lookback=2)


# ---------------------------------------------------------------------------
# expanding_regression_stats parity
# ---------------------------------------------------------------------------

def _run_expanding_parity(r_p_vals, r_b_vals, min_obs=2, prefix_none=True):
    prefix = [None] if prefix_none else []
    r_p_list = _series(prefix + r_p_vals)
    r_b_list = _series(prefix + r_b_vals)
    r_p_df = _df(prefix + r_p_vals)
    r_b_df = _df(prefix + r_b_vals)
    ref = expanding_regression_stats(r_p=r_p_list, r_b=r_b_list, min_obs=min_obs)
    got = my_expanding(r_p=r_p_df, r_b=r_b_df, min_obs=min_obs)
    assert len(got) == len(ref)
    for i, (g, r) in enumerate(zip(got, ref)):
        _assert_stats_match(g, r, i)


def test_expanding_matches_rolling_when_full_window():
    rets_p = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0]
    rets_b = [0.012, -0.018, 0.028, 0.006, -0.012, 0.024, 0.002]
    _run_expanding_parity(rets_p, rets_b)


def test_expanding_known_beta_parity():
    rng_r_b = [0.01, -0.02, 0.03, 0.005, -0.01, 0.02, 0.0, 0.015, -0.005, 0.01]
    rng_r_p = [1.5 * v + 0.001 for v in rng_r_b]
    _run_expanding_parity(rng_r_p, rng_r_b)


def test_expanding_skips_none_pairs():
    # Index 2 in r_p is None, index 5 in r_p is None → 4 valid pairs used
    r_p_df = pl.DataFrame(
        {"ts": [0, 1, 2, 3, 4, 5, 6], "ret": [None, 0.01, None, 0.03, 0.005, None, 0.02]},
        schema={"ts": pl.Int64, "ret": pl.Float64},
    )
    r_b_df = pl.DataFrame(
        {"ts": [0, 1, 2, 3, 4, 5, 6], "ret": [None, 0.02, 0.01, 0.06, 0.01, 0.03, 0.04]},
        schema={"ts": pl.Int64, "ret": pl.Float64},
    )
    r_p_list = _series([None, 0.01, None, 0.03, 0.005, None, 0.02])
    r_b_list = _series([None, 0.02, 0.01, 0.06, 0.01, 0.03, 0.04])
    ref = expanding_regression_stats(r_p=r_p_list, r_b=r_b_list)
    got = my_expanding(r_p=r_p_df, r_b=r_b_df)
    assert len(got) == len(ref)
    assert got[-1] is not None
    assert got[-1].n_obs == 4
    for i, (g, r) in enumerate(zip(got, ref)):
        _assert_stats_match(g, r, i)


def test_expanding_min_obs_less_than_two_raises():
    r_p_df = _df([None, 0.01, 0.02])
    r_b_df = _df([None, 0.01, 0.02])
    with pytest.raises(ValueError, match="min_obs"):
        my_expanding(r_p=r_p_df, r_b=r_b_df, min_obs=1)


def test_expanding_too_few_points_all_none():
    # Only 1 valid pair ever — all output None
    _run_expanding_parity([0.01], [0.02])


# ---------------------------------------------------------------------------
# degenerate / edge cases
# ---------------------------------------------------------------------------

def test_rolling_empty_series():
    r_p_df = _df([])
    r_b_df = _df([])
    got = my_rolling(r_p=r_p_df, r_b=r_b_df, lookback=2)
    assert got == []


def test_expanding_empty_series():
    r_p_df = _df([])
    r_b_df = _df([])
    got = my_expanding(r_p=r_p_df, r_b=r_b_df)
    assert got == []
