"""Headless parity proof for the total_return Python panel source.

Drives the decorated total_return source through the REAL /compute path
(in-process ComputeServer + stubbed rwclient serving canned NAV + flows rows)
and asserts the returned scalar equals the value computed by calling the metric
library DIRECTLY on the same canned data.

The algorithm is the same as the numbat total_return panel
(the metric reference):
  1. Clamp the effective window start to max(window_from, first_nav_ts).
  2. Build a daily grid from effective_start to window_end.
  3. Compute the cumulative TWR series (flows_after_effective_start).
  4. Return the final value of that series.

A literal numbat-vs-Python run requires the `n` binary and a live RW instance;
that comparison is the user's live acceptance step (see docs/reference-dashboards/
live-acceptance.md).
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

from compute.metrics import build_grid, cumulative_twr
from compute.server import ComputeServer
from compute import rwclient
from compute.rwclient import _frame_from


# ---------------------------------------------------------------------------
# Canned data
#
# Two NAV segments separated by one cash flow, with the window starting BEFORE
# the first NAV tick so the effective-start clamping is exercised.
#
# Timeline (µs; 1 day = 86_400_000_000):
#   t = -1d  (window start — before portfolio inception)
#   t =  0   NAV 1000.0   (portfolio inception — effective_start clamps here)
#   t =  1d  NAV 1050.0   (pre-flow)
#   t =  1d  FLOW
#   t =  1d  NAV 1600.0   (post-flow)
#   t =  2d  NAV 1664.0
#   t =  3d  NAV 1730.56  (window end)
#
# The expected total return is computed directly from the metric library in the
# assertion below, not by hand: the two NAV rows at t=1d (pre/post flow) make a
# hand-worked figure error-prone.
# ---------------------------------------------------------------------------

_D = 86_400_000_000  # 1 day in microseconds

_NAV_ROWS = {
    "columns": ["org_id", "portfolio", "ts", "value"],
    "rows": [
        ["org-1", "port-A", 0,      1000.0],
        ["org-1", "port-A", _D,     1050.0],
        ["org-1", "port-A", _D,     1600.0],    # post-flow NAV (after() picks this)
        ["org-1", "port-A", 2 * _D, 1664.0],
        ["org-1", "port-A", 3 * _D, 1730.56],
    ],
}

_FLOWS_ROWS = {
    "columns": ["org_id", "portfolio", "ts", "flow_type", "amt"],
    "rows": [["org-1", "port-A", _D, "DEPOSIT", 600.0]],
}

# Window deliberately opens one day BEFORE portfolio inception to exercise the
# effective-start clamp (max(t0, first_nav_ts) = max(-1d, 0) = 0).
_WINDOW = {"from": -_D, "to": 3 * _D}

# The reference panel source — uses sql() to pull data (new contract).
_SOURCE = r"""
@metric(output="scalar")
def total_return():
    nav = sql("SELECT * FROM nav WHERE portfolio = $1", "port-A")
    flows = sql("SELECT * FROM flows WHERE portfolio = $1", "port-A")
    t0, t1 = window
    effective_start = t0 if nav.is_empty() else max(t0, nav["ts"][0])
    flow_ts = [r[0] for r in flows.select("ts").rows() if r[0] > effective_start]
    grid = build_grid(effective_start, t1, "1d")
    cum = cumulative_twr(nav, flow_ts, effective_start, grid)
    return cum[-1][1] if cum else None
"""


# ---------------------------------------------------------------------------
# rwclient stub + compute server helpers
# ---------------------------------------------------------------------------

class _FakeConn:
    """Stub pg8000 connection for tests — has a no-op close()."""
    def close(self):
        pass


def _make_query_stub():
    def _query(conn, sql: str, params: tuple) -> pl.DataFrame:
        if "nav" in sql:
            return _frame_from(_NAV_ROWS["columns"], _NAV_ROWS["rows"])
        if "flows" in sql:
            return _frame_from(_FLOWS_ROWS["columns"], _FLOWS_ROWS["rows"])
        return pl.DataFrame()
    return _query


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


def _serve_compute():
    port = _free_port()
    server = ComputeServer(host="127.0.0.1", port=port, dsn="dsn://stub")
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
# Direct library reference — same algorithm as numbat total_return
# ---------------------------------------------------------------------------

def _nav_df() -> pl.DataFrame:
    return _frame_from(_NAV_ROWS["columns"], _NAV_ROWS["rows"])


def _expected_total_return() -> float | None:
    nav = _nav_df()
    t0, t1 = _WINDOW["from"], _WINDOW["to"]
    effective_start = t0 if nav.is_empty() else max(t0, nav["ts"][0])
    ts_idx = _FLOWS_ROWS["columns"].index("ts")
    all_flow_ts = [r[ts_idx] for r in _FLOWS_ROWS["rows"]]
    flow_ts = [f for f in all_flow_ts if f > effective_start]
    grid = build_grid(effective_start, t1, "1d")
    cum = cumulative_twr(nav, flow_ts, effective_start, grid)
    return cum[-1][1] if cum else None


# ---------------------------------------------------------------------------
# Parity test
# ---------------------------------------------------------------------------

def test_parity_total_return(monkeypatch) -> None:
    """total_return panel source round-trips through real /compute and matches direct lib.

    Proves the decorated Python source computes the same value as calling
    build_grid + cumulative_twr directly on the same canned NAV/flows data —
    the same algorithm the numbat total_return panel uses (cum_twr_grid →
    total_return(cum)).

    The stub rwclient serves canned rows; ComputeServer runs in-process.
    Effective-start clamping is exercised (window opens before portfolio inception).
    """
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", _make_query_stub())
    srv, base = _serve_compute()
    try:
        status, body = _post_compute(base, {"source": _SOURCE, "window": _WINDOW})
    finally:
        srv.shutdown()

    assert status == 200, f"expected 200, got {status}: {body}"
    assert body["output"] == "scalar"
    assert body["columns"] == ["value"]
    assert len(body["rows"]) == 1 and len(body["rows"][0]) == 1

    expected = _expected_total_return()
    assert expected is not None, "reference computation returned None — check canned data"

    got = body["rows"][0][0]
    assert got == pytest.approx(expected, rel=1e-9), (
        f"HTTP path returned {got!r}, direct lib returned {expected!r}"
    )
