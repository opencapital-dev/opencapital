"""P1 wiring acceptance — register -> fetch -> call -> frame vs. direct library.

Proves the FULL compute path (ComputeServer + stub /v1/rows + real metric library)
produces the same value as calling the metric library directly on the same canned
rows — i.e. every layer of P1 is wired correctly end-to-end.

Two tests:
  test_acceptance_scalar  — total return (TWR scalar) via a non-trivial series
                            with one external flow.
  test_acceptance_series  — equity curve (cumulative_twr series) over the same data.

Stub and server helpers are copied from test_endpoint.py; the canned data is
intentionally richer than the minimal endpoint tests (3 NAV points + 1 flow).
"""

from __future__ import annotations

import json
import socket
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import polars as pl
import pytest

from compute.metrics import (
    twr, build_grid, cumulative_twr,
    cumulative_to_period_returns, rolling_regression_stats,
    xirr,
)
from compute.server import ComputeServer


# ---------------------------------------------------------------------------
# Stub gateway — identical pattern to test_endpoint.py
# ---------------------------------------------------------------------------

def _make_gateway(rows_by_selector: dict):
    class _Stub(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(length))
            selector = req["selector"]
            doc = rows_by_selector.get(selector, {"columns": ["ts"], "rows": []})
            body = json.dumps(doc).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a: object) -> None:
            pass

    return _Stub


def _serve_gateway(rows_by_selector: dict):
    server = ThreadingHTTPServer(("127.0.0.1", 0), _make_gateway(rows_by_selector))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_address[1]}"


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


def _serve_compute(gateway_url: str):
    port = _free_port()
    server = ComputeServer(host="127.0.0.1", port=port, gateway_url=gateway_url)
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

# Selectors as they appear stripped of @mode (what the stub matches on).
_SEL_NAV   = "nav{portfolio=\"$p\"}"
_SEL_FLOWS = "flows{portfolio=\"$p\"}"

_STUB_ROWS = {_SEL_NAV: _NAV_ROWS, _SEL_FLOWS: _FLOWS_ROWS}

# Window: full 3-day span.
_WINDOW = {"from": 0, "to": 3 * _D}


# ---------------------------------------------------------------------------
# Decorated panel sources
# ---------------------------------------------------------------------------

# Scalar: windowed TWR total return.
_SCALAR_SRC = r"""
@bind(nav="nav{portfolio=\"$p\"} @window", flows="flows{portfolio=\"$p\"} @window")
@metric(output="scalar")
def total_return(nav, flows):
    t0, t1 = window
    flow_ts = flows["ts"].to_list() if flows.height else []
    return twr(nav, flow_ts, t0, t1)
"""

# Series: daily equity curve via cumulative_twr.
_SERIES_SRC = r"""
@bind(nav="nav{portfolio=\"$p\"} @window", flows="flows{portfolio=\"$p\"} @window")
@metric(output="series")
def equity_curve(nav, flows):
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
    data = {col: [r[i] for r in rows] for i, col in enumerate(cols)}
    return pl.DataFrame(data, schema_overrides={"ts": pl.Int64})


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

def test_acceptance_scalar() -> None:
    """Full register->fetch->call->frame path for a TWR scalar matches direct lib."""
    gw, gw_url = _serve_gateway(_STUB_ROWS)
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_compute(base, {"source": _SCALAR_SRC, "jwt": "tok", "window": _WINDOW})
    finally:
        srv.shutdown()
        gw.shutdown()

    assert status == 200, f"expected 200, got {status}: {body}"
    assert body["output"] == "scalar"
    assert body["columns"] == ["value"]
    assert len(body["rows"]) == 1 and len(body["rows"][0]) == 1

    expected = _expected_scalar()
    assert body["rows"][0][0] == pytest.approx(expected, rel=1e-9), (
        f"HTTP path returned {body['rows'][0][0]!r}, direct lib returned {expected!r}"
    )


def test_acceptance_series() -> None:
    """Full register->fetch->call->frame path for cumulative_twr series matches direct lib."""
    gw, gw_url = _serve_gateway(_STUB_ROWS)
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_compute(base, {"source": _SERIES_SRC, "jwt": "tok", "window": _WINDOW})
    finally:
        srv.shutdown()
        gw.shutdown()

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
# list[tuple[int, float|None]] — exercising the new list[tuple] framing path.
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

_SEL_PORTFOLIO_CUM = "cumret{portfolio=\"$p\"}"
_SEL_BENCHMARK_CUM = "cumret{benchmark=\"$b\"}"

_BETA_STUB_ROWS = {
    _SEL_PORTFOLIO_CUM: _PORTFOLIO_CUM_ROWS,
    _SEL_BENCHMARK_CUM: _BENCHMARK_CUM_ROWS,
}

_BETA_WINDOW = {"from": 0, "to": 5 * _D6}

# The panel converts cumulative series → per-period return DataFrames, runs
# rolling regression with lookback=3, and returns the beta series directly as
# list[tuple[int, float|None]] — no manual pl.DataFrame wrapping.
_BETA_SRC = r"""
@bind(
    portfolio="cumret{portfolio=\"$p\"} @window",
    benchmark="cumret{benchmark=\"$b\"} @window",
)
@metric(output="series")
def rolling_beta(portfolio, benchmark):
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
    import polars as pl

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


def test_acceptance_beta_rolling_series() -> None:
    """Full compute path for rolling beta framed as list[tuple] matches direct lib."""
    gw, gw_url = _serve_gateway(_BETA_STUB_ROWS)
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_compute(
            base, {"source": _BETA_SRC, "jwt": "tok", "window": _BETA_WINDOW}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

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

_SEL_CASHFLOWS = "cashflow{portfolio=\"$p\"}"

_XIRR_STUB_ROWS = {_SEL_CASHFLOWS: _CASHFLOW_ROWS}

_XIRR_WINDOW = {"from": 0, "to": _XIRR_YEAR_US}

# The panel builds signed flows (deposits negative, receipt positive)
# and returns xirr(...) as a scalar directly.
_XIRR_SRC = r"""
@bind(flows="cashflow{portfolio=\"$p\"} @window")
@metric(output="scalar")
def portfolio_xirr(flows):
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


def test_acceptance_xirr_scalar() -> None:
    """Full compute path for xirr scalar matches direct lib call on same cashflows."""
    gw, gw_url = _serve_gateway(_XIRR_STUB_ROWS)
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_compute(
            base, {"source": _XIRR_SRC, "jwt": "tok", "window": _XIRR_WINDOW}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

    assert status == 200, f"expected 200, got {status}: {body}"
    assert body["output"] == "scalar"
    assert body["columns"] == ["value"]
    assert len(body["rows"]) == 1 and len(body["rows"][0]) == 1

    expected = _expected_xirr()
    assert expected is not None, "reference xirr returned None — check test data"
    assert body["rows"][0][0] == pytest.approx(expected, rel=1e-9), (
        f"HTTP path returned {body['rows'][0][0]!r}, direct lib returned {expected!r}"
    )
