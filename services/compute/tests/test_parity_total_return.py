"""Headless parity proof for the total_return Python panel source.

Drives the decorated total_return source through the REAL /compute path
(in-process ComputeServer + stub /v1/rows gateway serving canned NAV + flows
rows) and asserts the returned scalar equals the value computed by calling the
metric library DIRECTLY on the same canned data.

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
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import polars as pl
import pytest

from compute.metrics import build_grid, cumulative_twr
from compute.server import ComputeServer


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

_SEL_NAV   = "nav{portfolio=\"$portfolio_id\"}"
_SEL_FLOWS = "flows{portfolio=\"$portfolio_id\"}"

_STUB_ROWS = {_SEL_NAV: _NAV_ROWS, _SEL_FLOWS: _FLOWS_ROWS}

# Window deliberately opens one day BEFORE portfolio inception to exercise the
# effective-start clamp (max(t0, first_nav_ts) = max(-1d, 0) = 0).
_WINDOW = {"from": -_D, "to": 3 * _D}

# The reference panel source — identical to
# the metric reference with variable
# names matching the canned selectors.
_SOURCE = r"""
@bind(nav="nav{portfolio=\"$portfolio_id\"} @asof", flows="flows{portfolio=\"$portfolio_id\"} @window")
@metric(output="scalar")
def total_return(nav, flows):
    t0, t1 = window
    effective_start = t0 if nav.is_empty() else max(t0, nav["ts"][0])
    flow_ts = [r[0] for r in flows.select("ts").rows() if r[0] > effective_start]
    grid = build_grid(effective_start, t1, "1d")
    cum = cumulative_twr(nav, flow_ts, effective_start, grid)
    return cum[-1][1] if cum else None
"""


# ---------------------------------------------------------------------------
# Stub gateway + compute server (same pattern as test_acceptance.py)
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
# Direct library reference — same algorithm as numbat total_return
# ---------------------------------------------------------------------------

def _nav_df() -> pl.DataFrame:
    cols, rows = _NAV_ROWS["columns"], _NAV_ROWS["rows"]
    data = {col: [r[i] for r in rows] for i, col in enumerate(cols)}
    return pl.DataFrame(data, schema_overrides={"ts": pl.Int64})


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

def test_parity_total_return() -> None:
    """total_return panel source round-trips through real /compute and matches direct lib.

    Proves the decorated Python source computes the same value as calling
    build_grid + cumulative_twr directly on the same canned NAV/flows data —
    the same algorithm the numbat total_return panel uses (cum_twr_grid →
    total_return(cum)).

    The stub gateway serves canned rows; ComputeServer runs in-process.
    Effective-start clamping is exercised (window opens before portfolio inception).
    """
    gw, gw_url = _serve_gateway(_STUB_ROWS)
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_compute(base, {"source": _SOURCE, "jwt": "tok", "window": _WINDOW})
    finally:
        srv.shutdown()
        gw.shutdown()

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
