"""Regression guard: real /v1/rows view shape through gateway -> DataFrame -> metric.

The P2 test suite historically fed metric functions the REDUCED shape
(only [ts, value] or [ts]) rather than the real normalized view columns
that the read-gateway actually emits. This test pins the live contract
end-to-end so that any ts/column mismatch would surface here first.

Shapes verified:
  e_nav   : org_id, portfolio, ts (Int64 µs), value
  e_flows : org_id, portfolio, ts (Int64 µs), flow_type, amt

A test that passed under the old shape (ts as string, value column named
"nav") would FAIL this test because:
  - _frame_from forces ts to Int64, so a string ts would error on coerce
  - twr.asof filters pl.col("ts") <= t expecting integers
  - cumulative_twr reads col("value"), so a "nav" column would give None
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import polars as pl
import pytest

from compute.contract import Binding, Window
from compute.gateway import fetch_rows, _frame_from
from compute.metrics.twr import asof, twr
from compute.metrics.returns import cumulative_twr, build_grid


_D = 86_400_000_000  # 1 day in microseconds

# Timeline:
#   T0        NAV 1000.0  (start)
#   T0+1d     NAV 1050.0  (pre-flow — strictly before flow at T0+1d+1s)
#   T0+1d+1s  FLOW (deposit; post-flow NAV jumps to 1600.0 at next tick)
#   T0+2d     NAV 1664.0
#   T0+3d     NAV 1730.56 (end)
#
# Seg 1 [T0, T0+1d+1s]: nav_a=asof(T0)=1000, nav_b=before(T0+1d+1s)=1050 → r=0.05
# Seg 2 [T0+1d+1s, T0+3d]: nav_a=after(T0+1d+1s)=1600, nav_b=asof(T0+3d)=1730.56
#   → r = 1730.56/1600 - 1 = 0.08160
# TWR = 1.05 * 1.08160 - 1 = 0.13568

_T0   = 1_717_000_000_000_000
_FLOW = _T0 + _D + 1_000_000    # 1 day + 1 second after T0
_T1   = _T0 + 3 * _D

# Real e_nav shape: org_id, portfolio, ts (bigint µs), value
_E_NAV_PAYLOAD = {
    "columns": ["org_id", "portfolio", "ts", "value"],
    "rows": [
        ["org-1", "port-A", _T0,          1000.0],
        ["org-1", "port-A", _T0 + _D,     1050.0],   # pre-flow NAV (strictly before _FLOW)
        ["org-1", "port-A", _FLOW,         1600.0],  # post-flow NAV
        ["org-1", "port-A", _T0 + 2 * _D, 1664.0],
        ["org-1", "port-A", _T1,           1730.56],
    ],
}

# Real e_flows shape: org_id, portfolio, ts (bigint µs), flow_type, amt
_E_FLOWS_PAYLOAD = {
    "columns": ["org_id", "portfolio", "ts", "flow_type", "amt"],
    "rows": [
        ["org-1", "port-A", _FLOW, "DEPOSIT", 550.0],
    ],
}


# ---------------------------------------------------------------------------
# Stub gateway
# ---------------------------------------------------------------------------

_SEL_NAV   = "nav{portfolio=\"$p\"}"
_SEL_FLOWS = "flows{portfolio=\"$p\"}"

_STUB = {_SEL_NAV: _E_NAV_PAYLOAD, _SEL_FLOWS: _E_FLOWS_PAYLOAD}


def _make_handler(rows_by_selector: dict):
    class _Stub(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(length))
            doc = rows_by_selector.get(req["selector"], {"columns": ["ts"], "rows": []})
            body = json.dumps(doc).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a: object) -> None:
            pass

    return _Stub


def _serve(rows_by_selector: dict):
    server = ThreadingHTTPServer(("127.0.0.1", 0), _make_handler(rows_by_selector))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_address[1]}"


# ---------------------------------------------------------------------------
# T8: boundary tests
# ---------------------------------------------------------------------------

def test_frame_from_real_nav_shape_ts_is_int64() -> None:
    """_frame_from emits Int64 ts and a value column from the real e_nav payload."""
    cols, rows = _E_NAV_PAYLOAD["columns"], _E_NAV_PAYLOAD["rows"]
    df = _frame_from(cols, rows)

    # ts must be Int64 (integer µs), NOT datetime or string
    assert df.schema["ts"] == pl.Int64, f"expected Int64, got {df.schema['ts']}"
    assert "value" in df.columns
    assert "org_id" in df.columns
    assert "portfolio" in df.columns

    ts_vals = df["ts"].to_list()
    assert all(isinstance(t, int) for t in ts_vals), "ts values must be Python ints"
    assert ts_vals[0] == _T0


def test_frame_from_real_flows_shape_ts_is_int64() -> None:
    """_frame_from emits Int64 ts plus scope and payload cols from real e_flows payload."""
    cols, rows = _E_FLOWS_PAYLOAD["columns"], _E_FLOWS_PAYLOAD["rows"]
    df = _frame_from(cols, rows)

    assert df.schema["ts"] == pl.Int64
    assert set(df.columns) == {"org_id", "portfolio", "ts", "flow_type", "amt"}
    assert df["ts"][0] == _FLOW


def test_fetch_rows_real_nav_shape_via_stub() -> None:
    """fetch_rows through a stub returning real e_nav shape yields correct frame."""
    server, base_url = _serve(_STUB)
    try:
        binding = Binding(_SEL_NAV, "window")
        df = fetch_rows(base_url, "tok", binding, Window(_T0, _T1))
    finally:
        server.shutdown()

    assert df.schema["ts"] == pl.Int64
    assert "value" in df.columns
    assert "org_id" in df.columns
    assert df.height == 5


def test_twr_metric_over_real_nav_shape() -> None:
    """twr() computes correctly on a frame built from the real e_nav wire shape.

    Seg 1 [T0, _FLOW]: nav_a=asof(T0)=1000, nav_b=before(_FLOW)=1050 → r=0.05
    Seg 2 [_FLOW, T1]: nav_a=after(_FLOW)=1600, nav_b=asof(T1)=1730.56 → r=0.0816
    TWR = (1.05 * 1.0816) - 1 = 0.13568
    """
    cols, rows = _E_NAV_PAYLOAD["columns"], _E_NAV_PAYLOAD["rows"]
    nav = _frame_from(cols, rows)

    result = twr(nav, [_FLOW], _T0, _T1)

    assert result is not None, "twr returned None on real-shape frame"
    expected = (1.05 * (1730.56 / 1600)) - 1.0
    assert result == pytest.approx(expected, rel=1e-9)


def test_cumulative_twr_over_real_nav_shape() -> None:
    """cumulative_twr returns a sensible final value from a real e_nav shaped frame."""
    cols, rows = _E_NAV_PAYLOAD["columns"], _E_NAV_PAYLOAD["rows"]
    nav = _frame_from(cols, rows)

    grid = build_grid(_T0, _T1, "1d")
    cum = cumulative_twr(nav, [_FLOW], _T0, grid)

    assert cum, "cumulative_twr returned empty result"
    final_ts, final_val = cum[-1]
    assert final_ts == _T1
    assert final_val is not None, "final cumulative_twr value is None"
    expected_twr = (1.05 * (1730.56 / 1600)) - 1.0
    assert final_val == pytest.approx(expected_twr, rel=1e-9)


def test_asof_ignores_scope_columns() -> None:
    """asof() works correctly on a frame with extra scope columns (org_id, portfolio).

    Confirms the metric functions are not broken by extra columns in the frame.
    """
    cols, rows = _E_NAV_PAYLOAD["columns"], _E_NAV_PAYLOAD["rows"]
    nav = _frame_from(cols, rows)

    assert asof(nav, _T0) == pytest.approx(1000.0)
    assert asof(nav, _T0 - 1) is None
