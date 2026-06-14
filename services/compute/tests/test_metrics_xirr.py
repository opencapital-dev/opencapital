"""Tests for compute.metrics.xirr.

Conventions matched from the metric reference +
the metric reference

  * Day-count / year basis: ACT/365.25 — xirr_year = 365.25 days.
  * Time origin: t0 = timestamp of first cashflow; subsequent exponents are
    (ts_i - t0) in microseconds / (365.25 * 86400 * 1_000_000).
  * Sign convention: investor-out (deposit) is negative; investor-in
    (receipt / terminal NAV) is positive — same as Excel XIRR.
  * Solver: pure bisection on [-0.99, 10.0], 60 halvings (≈1e-18 precision);
    returns None if the bracket has no sign change (all-same-sign cashflows
    or insufficient flows).
  * Annualization of cumulative return:
    cumulative = (1 + xirr_annualized)^window_nyears - 1
    where window_nyears = (window_end - effective_start) / 365.25 days.

Input shape: list[tuple[int, float]]  —  (ts_us microsecond-epoch int, signed_amount).
"""

from __future__ import annotations

import math
from datetime import datetime, timezone

import pytest

from compute.metrics.xirr import xirr, xirr_cumulative, npv_at


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

XIRR_YEAR_US = int(365.25 * 86_400 * 1_000_000)


def us(year: int, month: int, day: int) -> int:
    """Return a microsecond-epoch int for the given UTC date at midnight."""
    return int(datetime(year, month, day, tzinfo=timezone.utc).timestamp() * 1_000_000)


def _npv(rate: float, flows: list[tuple[int, float]]) -> float:
    """Reference NPV at given rate, ACT/365.25 basis, anchored at first flow."""
    t0 = flows[0][0]
    return sum(
        amt / (1 + rate) ** ((ts - t0) / XIRR_YEAR_US)
        for ts, amt in flows
    )


# ---------------------------------------------------------------------------
# Defining-property: NPV(XIRR(flows), flows) ≈ 0
# ---------------------------------------------------------------------------

def test_npv_at_solved_rate_is_zero_single_year():
    """10%-return single-year case: NPV at solved rate must vanish."""
    flows = [(us(2024, 1, 1), -1000.0), (us(2025, 1, 1), 1100.0)]
    r = xirr(flows)
    assert r is not None
    assert abs(_npv(r, flows)) < 1e-6


def test_npv_at_solved_rate_is_zero_irregular():
    """Three-flow irregular case: NPV at solved rate must vanish."""
    flows = [
        (us(2024, 1, 1), -1000.0),
        (us(2024, 7, 1),   500.0),
        (us(2025, 1, 1),   700.0),
    ]
    r = xirr(flows)
    assert r is not None
    assert abs(_npv(r, flows)) < 1e-6


def test_npv_at_solved_rate_is_zero_multi_deposit():
    """Two-deposit, one terminal NAV: NPV at solved rate must vanish."""
    flows = [
        (us(2023,  1,  1), -5000.0),
        (us(2023,  7,  1), -2000.0),
        (us(2024,  1,  1),  8000.0),
    ]
    r = xirr(flows)
    assert r is not None
    assert abs(_npv(r, flows)) < 1e-6


def test_npv_helper_consistency():
    """npv_at() exported from the module must agree with the local _npv helper."""
    flows = [(us(2024, 1, 1), -1000.0), (us(2025, 1, 1), 1100.0)]
    r = 0.05
    assert npv_at(r, flows) == pytest.approx(_npv(r, flows), rel=1e-12)


# ---------------------------------------------------------------------------
# Analytically-known cases
# ---------------------------------------------------------------------------

def test_one_year_ten_percent():
    """−100 at t0, +110 at t0+1yr (365.25-day year) → XIRR ≈ 0.10.

    Because 2024 is a leap year (366 days) the answer is slightly below 0.10;
    tolerance of 1e-3 matches the numbat test_xirr.numbat assertion.
    """
    flows = [(us(2024, 1, 1), -100.0), (us(2025, 1, 1), 110.0)]
    r = xirr(flows)
    assert r is not None
    assert r == pytest.approx(0.10, abs=1e-3)


def test_one_year_ten_percent_non_leap():
    """A 365-day (non-leap) year under ACT/365.25 lands just above 0.10."""
    # 2019-01-01 → 2020-01-01 = 365 days (2019 is not a leap year)
    flows = [(us(2019, 1, 1), -1000.0), (us(2020, 1, 1), 1100.0)]
    r = xirr(flows)
    assert r is not None
    # ACT/365.25 exponent = 365/365.25 ≈ 0.999316; r slightly above 10%
    # NPV must still be near zero
    assert abs(_npv(r, flows)) < 1e-6
    assert r == pytest.approx(0.10, abs=2e-3)


def test_negative_return():
    """Loss scenario must return a negative rate."""
    flows = [(us(2024, 1, 1), -1000.0), (us(2025, 1, 1), 950.0)]
    r = xirr(flows)
    assert r is not None
    assert r < 0


def test_zero_return():
    """Return exactly the amount deposited → XIRR = 0."""
    flows = [(us(2024, 1, 1), -1000.0), (us(2024, 7, 1), 1000.0)]
    r = xirr(flows)
    assert r is not None
    assert r == pytest.approx(0.0, abs=1e-6)


def test_irregular_three_flow_numbat_case():
    """Mirror the numbat test_xirr.numbat case: r ≈ 0.2615 (±1e-3)."""
    flows = [
        (us(2024, 1, 1), -1000.0),
        (us(2024, 7, 1),   500.0),
        (us(2025, 1, 1),   700.0),
    ]
    r = xirr(flows)
    assert r is not None
    assert r == pytest.approx(0.2615, abs=1e-3)


# ---------------------------------------------------------------------------
# Day-count / year-basis convention check
# ---------------------------------------------------------------------------

def test_year_basis_is_365_25_not_365():
    """Demonstrate that the year basis used is 365.25, not 365.

    We set up a case where the two bases give results that differ by > 1e-4
    and confirm our answer matches the 365.25 side.
    """
    # 2019-01-01 → 2024-01-01 = 5 * 365 + 2 leap days = 1827 days
    t0 = us(2019, 1, 1)
    t1 = us(2024, 1, 1)
    days = (t1 - t0) / (86_400 * 1_000_000)  # 1827.0

    # Construct flows such that the 365.25-basis XIRR is exactly 0.10
    # => terminal = 1000 * 1.10^(days/365.25)
    terminal_365_25 = 1000.0 * (1.10 ** (days / 365.25))
    flows = [(t0, -1000.0), (t1, terminal_365_25)]

    r = xirr(flows)
    assert r is not None

    # At the 365.25-basis exact root, NPV must be ≈ 0
    assert abs(_npv(r, flows)) < 1e-6

    # Under a 365-day basis the exponent would be days/365 ≠ days/365.25.
    # The difference over 5 years is large enough to distinguish:
    r_365_basis = (terminal_365_25 / 1000.0) ** (365.0 / days) - 1.0  # ~0.1004
    assert abs(r - 0.10) < abs(r_365_basis - 0.10), (
        f"Rate {r:.6f} is closer to 0.10 under 365.25 basis than "
        f"the 365-day answer {r_365_basis:.6f}; confirms basis=365.25"
    )


# ---------------------------------------------------------------------------
# Annualized variant
# ---------------------------------------------------------------------------

def test_xirr_cumulative_one_year():
    """Cumulative return over exactly one year = annualized return."""
    # 365-day window so nyears ≈ 1; cumulative ≈ annualized
    flows = [(us(2019, 1, 1), -1000.0), (us(2020, 1, 1), 1100.0)]
    t_start = us(2019, 1, 1)
    t_end = us(2020, 1, 1)
    cum = xirr_cumulative(flows, window_start=t_start, window_end=t_end)
    ann = xirr(flows)
    assert cum is not None and ann is not None
    nyears = (t_end - t_start) / XIRR_YEAR_US
    expected = (1.0 + ann) ** nyears - 1.0
    assert cum == pytest.approx(expected, rel=1e-9)


def test_xirr_cumulative_multi_year():
    """Cumulative > annualized for multi-year positive-return window."""
    flows = [
        (us(2019, 1, 1), -1000.0),
        (us(2021, 1, 1),  1500.0),
    ]
    t_start = us(2019, 1, 1)
    t_end = us(2021, 1, 1)
    cum = xirr_cumulative(flows, window_start=t_start, window_end=t_end)
    ann = xirr(flows)
    assert cum is not None and ann is not None
    nyears = (t_end - t_start) / XIRR_YEAR_US
    expected = (1.0 + ann) ** nyears - 1.0
    assert cum == pytest.approx(expected, rel=1e-9)
    # Cumulative should be larger than annualized when ann > 0 and nyears > 1
    assert cum > ann


def test_xirr_cumulative_returns_none_on_degenerate():
    """xirr_cumulative returns None when xirr itself returns None."""
    flows = [(us(2024, 1, 1), -1000.0), (us(2025, 1, 1), -500.0)]
    t_start = us(2024, 1, 1)
    t_end = us(2025, 1, 1)
    result = xirr_cumulative(flows, window_start=t_start, window_end=t_end)
    assert result is None


# ---------------------------------------------------------------------------
# Degenerate inputs — must return None, never NaN/inf/wrong number
# ---------------------------------------------------------------------------

def test_all_negative_no_sign_change_returns_none():
    """All outflows — no sign change — no root in [-0.99, 10]; return None."""
    flows = [(us(2024, 1, 1), -1000.0), (us(2025, 1, 1), -500.0)]
    result = xirr(flows)
    assert result is None


def test_all_positive_no_sign_change_returns_none():
    """All inflows — no sign change — return None."""
    flows = [(us(2024, 1, 1), 1000.0), (us(2025, 1, 1), 500.0)]
    result = xirr(flows)
    assert result is None


def test_single_flow_returns_none():
    """Single cashflow — no equation to solve — return None."""
    result = xirr([(us(2024, 1, 1), -1000.0)])
    assert result is None


def test_empty_flows_returns_none():
    """Empty input — return None."""
    result = xirr([])
    assert result is None


def test_result_is_never_nan_or_inf():
    """Pathological flows must not produce NaN or inf; return None instead."""
    # Enormous amounts that might trigger numerical issues
    flows = [
        (us(2024, 1, 1), -1e15),
        (us(2024, 1, 2), -1e15),
    ]
    result = xirr(flows)
    if result is not None:
        assert math.isfinite(result), f"Expected finite or None, got {result}"
    # all-negative → should be None
    assert result is None


def test_near_zero_flows_degenerate():
    """Zero deposit + positive terminal: effectively all positive → None."""
    flows = [(us(2024, 1, 1), 0.0), (us(2025, 1, 1), 1000.0)]
    result = xirr(flows)
    # 0 + 1000 → both non-negative side → no sign change from [-0.99, 10]
    # NPV(lo) and NPV(hi) both positive → None
    assert result is None


def test_two_flows_same_timestamp_but_different_signs():
    """Both at t0: second term has exponent 0 so NPV is just sum of amounts."""
    # NPV = -1000 + 500 = -500 for ALL rates → no root → None
    t = us(2024, 1, 1)
    flows = [(t, -1000.0), (t, 500.0)]
    result = xirr(flows)
    assert result is None


# ---------------------------------------------------------------------------
# Window assembly convention (from portfolio_xirr_window.numbat)
# ---------------------------------------------------------------------------

def test_window_assembly_opening_nav_negative_terminal_positive():
    """Opening NAV is a synthetic deposit (negative) and terminal is positive.

    Mirrors the numbat convention:
      Flow { ts: effective_start, amt: -opening_nav }  →  negative
      Flow { ts: window_end,      amt: terminal_nav  }  →  positive
      External flows in investor POV (deposit=negative, withdrawal=positive)
    assembled and passed to xirr(). Here we round-trip with a known answer.
    """
    opening_nav = 10_000.0
    terminal_nav = 12_000.0
    t_start = us(2024, 1, 1)
    t_end = us(2025, 1, 1)

    # Only opening + closing NAV, no intermediate flows
    flows = [
        (t_start, -opening_nav),
        (t_end,    terminal_nav),
    ]
    r = xirr(flows)
    assert r is not None
    # NPV must vanish
    assert abs(_npv(r, flows)) < 1e-6
    # Rough sanity: ~20% gain over ~1 year
    assert r == pytest.approx(0.20, abs=5e-3)


# ---------------------------------------------------------------------------
# Solver robustness
# ---------------------------------------------------------------------------

def test_solver_handles_high_return():
    """Solver finds rates up to the bracket ceiling (10.0 = 1000% annualized).

    A 5x return over 6 months is ~2400% annualized — above the numbat
    bracket [-0.99, 10.0], so no sign change exists and None is correct.
    A 1000% return over 1 year sits just inside the bracket.
    """
    # 5x in 6 months → XIRR ~2400%; bracket [-0.99, 10.0] misses it → None
    flows_out_of_bracket = [
        (us(2024, 1, 1), -1000.0),
        (us(2024, 7, 1), 5000.0),
    ]
    assert xirr(flows_out_of_bracket) is None

    # 1000% over 1 year: just inside Hi=10 boundary → converges
    flows_inside = [(us(2024, 1, 1), -1000.0), (us(2025, 1, 1), 10_000.0)]
    r = xirr(flows_inside)
    assert r is not None
    assert r > 1.0
    assert abs(_npv(r, flows_inside)) < 1e-6


def test_solver_handles_large_loss():
    """Total loss scenario: rate approaches -99%."""
    flows = [(us(2024, 1, 1), -1000.0), (us(2025, 1, 1), 10.0)]
    r = xirr(flows)
    assert r is not None
    assert r < -0.9
    assert abs(_npv(r, flows)) < 1e-6


def test_many_flows_convergence():
    """Monthly deposits over two years + terminal NAV must converge."""
    flows = []
    for i in range(24):
        ts = us(2022 + i // 12, (i % 12) + 1, 1)
        flows.append((ts, -500.0))  # monthly deposit
    flows.append((us(2024, 1, 1), 13_000.0))  # terminal NAV
    r = xirr(flows)
    assert r is not None
    assert abs(_npv(r, flows)) < 1e-4  # looser tolerance for many flows
