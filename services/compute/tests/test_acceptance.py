"""P1 wiring acceptance — register -> sql() -> call -> frame vs. direct library.

Proves the FULL compute path (ComputeServer + stubbed rwclient + real metric library)
produces the same value as calling the metric library directly on the same canned
rows — i.e. every layer of P1 is wired correctly end-to-end.

Two tests:
  test_acceptance_scalar  — total return (TWR scalar) via a non-trivial series
                            with one external flow.
  test_acceptance_series  — equity curve (cumulative_twr series) over the same data.

The stub patches ``rwclient.connect`` / ``rwclient.query`` so no live RisingWave
is needed; the metric source calls sql() directly in its body.
"""

from __future__ import annotations

import json
import socket
import threading
import time
import urllib.error
import urllib.request

import polars as pl
import pytest

from compute.metrics import (
    twr, build_grid, cumulative_twr,
    cumulative_to_period_returns, rolling_regression_stats,
    xirr,
)
from compute.server import ComputeServer
from compute import rwclient
from compute.rwclient import _frame_from


# ---------------------------------------------------------------------------
# Canned data — 4 NAV points + 1 external cash flow (mid-window)
#
# Timeline (microseconds, 1 day = 86_400_000_000 µs):
#   t=0   NAV  1000.0   (start)
#   t=1d  NAV  1050.0   (pre-flow)
#   t=1d  FLOW (cash injection; NAV jumps to 1600.0 immediately after)
#   t=2d  NAV  1632.0
#   t=3d  NAV  1697.28  (end)
#
# Segment 1: [0, 1d]  → 1050/1000 - 1 = 0.05
# Segment 2: [1d, 3d] → 1697.28/1600 - 1 = 0.0608
# TWR = (1.05 * 1.0608) - 1 = 0.11384
# ---------------------------------------------------------------------------

_D = 86_400_000_000  # 1 day in microseconds

_NAV_ROWS = {
    "columns": ["org_id", "portfolio", "ts", "value"],
    "rows": [
        ["org-1", "port-A", 0,      1000.0],
        ["org-1", "port-A", _D,     1050.0],
        ["org-1", "port-A", _D,     1600.0],   # post-flow NAV at same timestamp (after() picks this)
        ["org-1", "port-A", 2 * _D, 1632.0],
        ["org-1", "port-A", 3 * _D, 1697.28],
    ],
}

_FLOWS_ROWS = {
    "columns": ["org_id", "portfolio", "ts", "flow_type", "amt"],
    "rows": [["org-1", "port-A", _D, "DEPOSIT", 600.0]],   # one external flow at t=1d
}

# Window: full 3-day span.
_WINDOW = {"from": 0, "to": 3 * _D}


# ---------------------------------------------------------------------------
# rwclient stub helpers
# ---------------------------------------------------------------------------

class _FakeConn:
    """Stub pg8000 connection for tests — has a no-op close()."""
    def close(self):
        pass


def _make_query_stub(table_data: dict[str, dict]):
    """Return a query stub that maps a simple table name to canned rows.

    The stub inspects the SQL query for known table names (nav, flows, etc.)
    and returns the matching canned frame.  Params are ignored.
    """
    def _query(conn, sql: str, params: tuple) -> pl.DataFrame:
        for name, doc in table_data.items():
            if name in sql:
                return _frame_from(doc["columns"], doc["rows"])
        return pl.DataFrame()
    return _query


# ---------------------------------------------------------------------------
# Compute server helpers
# ---------------------------------------------------------------------------

def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait_ready(url: str, timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            urllib.request.urlopen(url, timeout=0.2)
            return
        except Exception:
            time.sleep(0.05)
    raise TimeoutError(f"server not ready at {url}")


def _serve_compute(dsn: str = "dsn://stub"):
    port = _free_port()
    server = ComputeServer(host="127.0.0.1", port=port, dsn=dsn)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{port}"
    _wait_ready(base + "/health")
    return server, base


def _post_compute(base: str, body: dict) -> tuple[int, dict]:
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        base + "/compute", data=data, method="POST",
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


# ---------------------------------------------------------------------------
# Decorated panel sources (new sql() contract)
# ---------------------------------------------------------------------------

# Scalar: windowed TWR total return.
_SCALAR_SRC = r"""
@metric(output="scalar")
def total_return():
    nav = sql("SELECT * FROM nav WHERE portfolio = $1", "port-A")
    flows = sql("SELECT * FROM flows WHERE portfolio = $1", "port-A")
    t0, t1 = window
    flow_ts = flows["ts"].to_list() if flows.height else []
    return twr(nav, flow_ts, t0, t1)
"""

# Series: daily equity curve via cumulative_twr.
_SERIES_SRC = r"""
@metric(output="series")
def equity_curve():
    nav = sql("SELECT * FROM nav WHERE portfolio = $1", "port-A")
    flows = sql("SELECT * FROM flows WHERE portfolio = $1", "port-A")
    t0, t1 = window
    flow_ts = flows["ts"].to_list() if flows.height else []
    grid = build_grid(t0, t1, "1d")
    pts = cumulative_twr(nav, flow_ts, t0, grid)
    return pl.DataFrame({"ts": [p[0] for p in pts], "value": [p[1] for p in pts]})
"""


# ---------------------------------------------------------------------------
# Direct library reference — same computation on the same canned rows.
# These produce the "expected" values that the HTTP path must match.
# ---------------------------------------------------------------------------

def _nav_df() -> pl.DataFrame:
    cols, rows = _NAV_ROWS["columns"], _NAV_ROWS["rows"]
    return _frame_from(cols, rows)


def _flow_ts() -> list[int]:
    ts_idx = _FLOWS_ROWS["columns"].index("ts")
    return [r[ts_idx] for r in _FLOWS_ROWS["rows"]]


def _expected_scalar() -> float:
    return twr(_nav_df(), _flow_ts(), 0, 3 * _D)


def _expected_series() -> list[tuple[int, float]]:
    grid = build_grid(0, 3 * _D, "1d")
    return cumulative_twr(_nav_df(), _flow_ts(), 0, grid)


# ---------------------------------------------------------------------------
# Acceptance tests
# ---------------------------------------------------------------------------

def test_acceptance_scalar(monkeypatch) -> None:
    """Full register->sql()->call->frame path for a TWR scalar matches direct lib."""
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", _make_query_stub({"nav": _NAV_ROWS, "flows": _FLOWS_ROWS}))
    srv, base = _serve_compute()
    try:
        status, body = _post_compute(base, {"source": _SCALAR_SRC, "window": _WINDOW})
    finally:
        srv.shutdown()

    assert status == 200, f"expected 200, got {status}: {body}"
    assert body["output"] == "scalar"
    assert body["columns"] == ["value"]
    assert len(body["rows"]) == 1 and len(body["rows"][0]) == 1

    expected = _expected_scalar()
    assert body["rows"][0][0] == pytest.approx(expected, rel=1e-9), (
        f"HTTP path returned {body['rows'][0][0]!r}, direct lib returned {expected!r}"
    )


def test_acceptance_series(monkeypatch) -> None:
    """Full register->sql()->call->frame path for cumulative_twr series matches direct lib."""
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", _make_query_stub({"nav": _NAV_ROWS, "flows": _FLOWS_ROWS}))
    srv, base = _serve_compute()
    try:
        status, body = _post_compute(base, {"source": _SERIES_SRC, "window": _WINDOW})
    finally:
        srv.shutdown()

    assert status == 200, f"expected 200, got {status}: {body}"
    assert body["output"] == "series"
    assert body["columns"] == ["ts", "value"]

    expected_pts = _expected_series()
    assert len(body["rows"]) == len(expected_pts)

    for i, ((exp_ts, exp_val), row) in enumerate(zip(expected_pts, body["rows"])):
        got_ts, got_val = row
        assert got_ts == exp_ts, f"row {i}: ts mismatch {got_ts} vs {exp_ts}"
        assert got_val == pytest.approx(exp_val, rel=1e-9), (
            f"row {i} (ts={exp_ts}): value {got_val!r} vs expected {exp_val!r}"
        )


# ---------------------------------------------------------------------------
# Beta e2e — rolling_regression_stats through the full compute path.
#
# Two equity curves (portfolio + benchmark) with known daily returns.
# The panel calls cumulative_to_period_returns, builds r_p/r_b DataFrames,
# calls rolling_regression_stats, and returns the beta series as
# list[tuple[int, float|None]] — exercising the list[tuple] framing path.
# ---------------------------------------------------------------------------

# Daily cumulative returns for 6 days.
# Portfolio : 0%, 2%, 5%, 3%, 8%, 6%
# Benchmark : 0%, 1%, 3%, 2%, 5%, 4%
_D6 = 86_400_000_000  # 1 day µs

_PORTFOLIO_CUM_ROWS = {
    "columns": ["ts", "value"],
    "rows": [
        [0 * _D6, 0.00],
        [1 * _D6, 0.02],
        [2 * _D6, 0.05],
        [3 * _D6, 0.03],
        [4 * _D6, 0.08],
        [5 * _D6, 0.06],
    ],
}

_BENCHMARK_CUM_ROWS = {
    "columns": ["ts", "value"],
    "rows": [
        [0 * _D6, 0.00],
        [1 * _D6, 0.01],
        [2 * _D6, 0.03],
        [3 * _D6, 0.02],
        [4 * _D6, 0.05],
        [5 * _D6, 0.04],
    ],
}

_BETA_WINDOW = {"from": 0, "to": 5 * _D6}

# The panel converts cumulative series → per-period return DataFrames, runs
# rolling regression with lookback=3, and returns the beta series directly as
# list[tuple[int, float|None]] — no manual pl.DataFrame wrapping.
_BETA_SRC = r"""
@metric(output="series")
def rolling_beta():
    portfolio = sql("SELECT * FROM portfolio_cum WHERE portfolio = $1", "port-A")
    benchmark = sql("SELECT * FROM benchmark_cum WHERE benchmark = $1", "bench-X")

    # cumulative_to_period_returns expects list[tuple[int, float|None]]
    port_cum = portfolio.rows()
    bench_cum = benchmark.rows()

    port_per = cumulative_to_period_returns(port_cum)
    bench_per = cumulative_to_period_returns(bench_cum)

    r_p = pl.DataFrame(
        {"ts": [t for t, _ in port_per], "ret": [r for _, r in port_per]},
        schema={"ts": pl.Int64, "ret": pl.Float64},
    )
    r_b = pl.DataFrame(
        {"ts": [t for t, _ in bench_per], "ret": [r for _, r in bench_per]},
        schema={"ts": pl.Int64, "ret": pl.Float64},
    )

    stats = rolling_regression_stats(r_p=r_p, r_b=r_b, lookback=3)
    ts_col = portfolio["ts"].to_list()
    return [(ts_col[i], s.beta if s is not None else None) for i, s in enumerate(stats)]
"""


def _expected_beta_series() -> list[tuple[int, float | None]]:
    """Direct library call on the same canned data."""
    port_rows = _PORTFOLIO_CUM_ROWS["rows"]
    bench_rows = _BENCHMARK_CUM_ROWS["rows"]

    port_cum = [(r[0], r[1]) for r in port_rows]
    bench_cum = [(r[0], r[1]) for r in bench_rows]

    port_per = cumulative_to_period_returns(port_cum)
    bench_per = cumulative_to_period_returns(bench_cum)

    r_p = pl.DataFrame(
        {"ts": [t for t, _ in port_per], "ret": [r for _, r in port_per]},
        schema={"ts": pl.Int64, "ret": pl.Float64},
    )
    r_b = pl.DataFrame(
        {"ts": [t for t, _ in bench_per], "ret": [r for _, r in bench_per]},
        schema={"ts": pl.Int64, "ret": pl.Float64},
    )

    stats = rolling_regression_stats(r_p=r_p, r_b=r_b, lookback=3)
    ts_col = [r[0] for r in port_rows]
    return [(ts_col[i], s.beta if s is not None else None) for i, s in enumerate(stats)]


def _make_beta_query_stub():
    def _query(conn, sql: str, params: tuple) -> pl.DataFrame:
        if "portfolio_cum" in sql:
            return _frame_from(_PORTFOLIO_CUM_ROWS["columns"], _PORTFOLIO_CUM_ROWS["rows"])
        if "benchmark_cum" in sql:
            return _frame_from(_BENCHMARK_CUM_ROWS["columns"], _BENCHMARK_CUM_ROWS["rows"])
        return pl.DataFrame()
    return _query


def test_acceptance_beta_rolling_series(monkeypatch) -> None:
    """Full compute path for rolling beta framed as list[tuple] matches direct lib."""
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", _make_beta_query_stub())
    srv, base = _serve_compute()
    try:
        status, body = _post_compute(
            base, {"source": _BETA_SRC, "window": _BETA_WINDOW}
        )
    finally:
        srv.shutdown()

    assert status == 200, f"expected 200, got {status}: {body}"
    assert body["output"] == "series"
    assert body["columns"] == ["ts", "value"]

    expected = _expected_beta_series()
    assert len(body["rows"]) == len(expected)

    for i, ((exp_ts, exp_beta), row) in enumerate(zip(expected, body["rows"])):
        got_ts, got_beta = row
        assert got_ts == exp_ts, f"row {i}: ts mismatch {got_ts} vs {exp_ts}"
        if exp_beta is None:
            assert got_beta is None, f"row {i}: expected None, got {got_beta}"
        else:
            assert got_beta == pytest.approx(exp_beta, rel=1e-9), (
                f"row {i}: beta {got_beta!r} vs expected {exp_beta!r}"
            )


# ---------------------------------------------------------------------------
# XIRR e2e — xirr() scalar through the full compute path.
#
# A simple 3-cashflow portfolio: invest 1000 at t=0, add 500 at t=6m,
# receive terminal value 1700 at t=1y.  The panel builds signed flows
# and returns xirr(...) directly as a scalar.
# ---------------------------------------------------------------------------

_XIRR_YEAR_US = int(365.25 * 86_400 * 1_000_000)
_6M_US = _XIRR_YEAR_US // 2

_CASHFLOW_ROWS = {
    "columns": ["ts", "amount"],
    "rows": [
        [0,          1000.0],   # deposit
        [_6M_US,      500.0],   # deposit
        [_XIRR_YEAR_US, 1700.0],  # terminal receipt
    ],
}

_XIRR_WINDOW = {"from": 0, "to": _XIRR_YEAR_US}

# The panel builds signed flows (deposits negative, receipt positive)
# and returns xirr(...) as a scalar directly.
_XIRR_SRC = r"""
@metric(output="scalar")
def portfolio_xirr():
    flows = sql("SELECT * FROM cashflow WHERE portfolio = $1", "port-A")
    ts_col = flows["ts"].to_list()
    amt_col = flows["amount"].to_list()
    n = len(ts_col)
    # Deposits are negative (money out), final receipt is positive.
    signed = [
        (ts_col[i], -amt_col[i] if i < n - 1 else amt_col[i])
        for i in range(n)
    ]
    return xirr(signed)
"""


def _expected_xirr() -> float | None:
    """Direct library call on the same canned cashflows."""
    rows = _CASHFLOW_ROWS["rows"]
    ts_col = [r[0] for r in rows]
    amt_col = [r[1] for r in rows]
    n = len(ts_col)
    signed = [
        (ts_col[i], -amt_col[i] if i < n - 1 else amt_col[i])
        for i in range(n)
    ]
    return xirr(signed)


def test_acceptance_xirr_scalar(monkeypatch) -> None:
    """Full compute path for xirr scalar matches direct lib call on same cashflows."""
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", _make_query_stub({"cashflow": _CASHFLOW_ROWS}))
    srv, base = _serve_compute()
    try:
        status, body = _post_compute(
            base, {"source": _XIRR_SRC, "window": _XIRR_WINDOW}
        )
    finally:
        srv.shutdown()

    assert status == 200, f"expected 200, got {status}: {body}"
    assert body["output"] == "scalar"
    assert body["columns"] == ["value"]
    assert len(body["rows"]) == 1 and len(body["rows"][0]) == 1

    expected = _expected_xirr()
    assert expected is not None, "reference xirr returned None — check test data"
    assert body["rows"][0][0] == pytest.approx(expected, rel=1e-9), (
        f"HTTP path returned {body['rows'][0][0]!r}, direct lib returned {expected!r}"
    )
