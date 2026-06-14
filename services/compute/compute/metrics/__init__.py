"""compute.metrics — financial metric functions (standalone, no common/ dependency).

``__all__`` is the panel-facing surface: exactly the names the ``/compute``
endpoint injects into a source's exec namespace.  Internal accumulators
(``RegressionStats``, ``npv_at``) stay unexported.
"""

from compute.metrics.twr import asof, after, before, bounds, seg, twr
from compute.metrics.returns import build_grid, cumulative_simple_return, cumulative_twr, sample_at_grid, cum_returns_from_navs
from compute.metrics.beta import (
    cumulative_to_period_returns,
    cum_to_period_returns,
    expanding_regression_stats,
    rolling_regression_stats,
)
from compute.metrics.xirr import xirr, xirr_cumulative
from compute.metrics.drawdown import max_drawdown, current_drawdown, drawdown_episodes, DrawdownEpisode
from compute.metrics.riskstats import (
    annualize_return,
    volatility_annualized,
    sharpe,
    sortino,
    calmar,
)
from compute.metrics.positions import positions

__all__ = [
    "asof", "after", "before", "bounds", "seg", "twr",
    "build_grid", "cumulative_simple_return", "cumulative_twr",
    "sample_at_grid", "cum_returns_from_navs",
    "cumulative_to_period_returns", "rolling_regression_stats",
    "expanding_regression_stats",
    "xirr", "xirr_cumulative",
    "max_drawdown", "current_drawdown", "drawdown_episodes", "DrawdownEpisode",
    "cum_to_period_returns", "annualize_return", "volatility_annualized",
    "sharpe", "sortino", "calmar",
    "positions",
]
