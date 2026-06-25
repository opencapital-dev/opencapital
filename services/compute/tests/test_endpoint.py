"""``POST /compute`` endpoint tests — register -> call -> frame (pgwire edition).

Tests monkeypatch ``rwclient.connect`` / ``rwclient.query`` so no live
RisingWave is needed.  Covers the neutral frame per output mode, the injected
namespace surface, error shapes, per-request isolation, and the _to_frame
helpers.
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

from compute.contract import Window, make_contract
from compute.endpoint import _to_frame, build_namespace, run_compute
from compute import endpoint as ep, rwclient
from compute.server import ComputeServer

# keep short alias for existing tests that reference `endpoint.*`
endpoint = ep


# ---------------------------------------------------------------------------
# Helpers: spin up a ComputeServer with stubbed rwclient
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
# Core unit test: run_compute stubs pgwire (no server needed)
# ---------------------------------------------------------------------------

class _FakeConn:
    """Stub pg8000 connection for tests — has a no-op close()."""
    def close(self):
        pass


class _FakeStore:
    """Stub Store for endpoint tests — returns a fixed DataFrame from run()."""
    def __init__(self, *a, **k):
        pass

    def run(self, spec):
        return pl.DataFrame({"ts": [1, 2], "nav": [100.0, 110.0]})

    def close(self):
        pass


def test_run_compute_calls_sql_and_frames_result(monkeypatch):
    """Verify the sql() convenience in the exec namespace calls through the store."""
    class _SqlStore:
        def __init__(self, *a, **k): pass
        def run(self, spec): return pl.DataFrame({"ts": [1, 2], "nav": [10.0, 11.0]})
        def close(self): pass

    monkeypatch.setattr(ep, "Store", _SqlStore)
    source = (
        "@metric(output='series')\n"
        "def m():\n"
        "    df = sql('SELECT ts, nav FROM nav WHERE portfolio = $1', 'p1')\n"
        "    return df\n"
    )
    out = endpoint.run_compute({"source": source, "window": {"from": 0, "to": 9}}, "dsn", None)
    assert out["columns"] == ["ts", "nav"]
    assert out["rows"] == [[1, 10.0], [2, 11.0]]


def test_run_compute_scalar(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p: pl.DataFrame({"v": [42.0]}))
    source = (
        "@metric(output='scalar')\n"
        "def m():\n"
        "    return 3.14\n"
    )
    out = endpoint.run_compute({"source": source, "window": {"from": 0, "to": 1}}, "dsn", None)
    assert out["output"] == "scalar"
    assert out["rows"] == [[3.14]]


def test_run_compute_source_error_is_400(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    source = "raise ValueError('bad source')"
    from compute.endpoint import ComputeError
    with pytest.raises(ComputeError) as exc_info:
        endpoint.run_compute({"source": source, "window": {"from": 0, "to": 1}}, "dsn", None)
    assert exc_info.value.status == 400
    assert "bad source" in exc_info.value.message


def test_run_compute_entrypoint_error_is_400(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    source = (
        "@metric(output='scalar')\n"
        "def m():\n"
        "    raise RuntimeError('boom')\n"
    )
    from compute.endpoint import ComputeError
    with pytest.raises(ComputeError) as exc_info:
        endpoint.run_compute({"source": source, "window": {"from": 0, "to": 1}}, "dsn", None)
    assert exc_info.value.status == 400
    assert "boom" in exc_info.value.message


def test_run_compute_no_metric_is_400(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    source = "x = 1  # no @metric anywhere"
    from compute.endpoint import ComputeError
    with pytest.raises(ComputeError) as exc_info:
        endpoint.run_compute({"source": source, "window": {"from": 0, "to": 1}}, "dsn", None)
    assert exc_info.value.status == 400
    assert "@metric" in exc_info.value.message


def test_run_compute_binds_frames_to_entrypoint(monkeypatch):
    monkeypatch.setattr(ep, "Store", _FakeStore)
    src = (
        "@bind(nav=rw('SELECT ts, nav FROM portfolio_per_tick WHERE portfolio_id=$1', 'p1'))\n"
        "@metric(output='scalar')\n"
        "def m(nav):\n"
        "    return float(nav['nav'][-1])\n"
    )
    out = run_compute({"source": src, "window": {"from": 0, "to": 100}}, "rwdsn", "pgdsn")
    assert out["output"] == "scalar"
    assert out["rows"] == [[110.0]]


# ---------------------------------------------------------------------------
# Namespace surface
# ---------------------------------------------------------------------------

def test_namespace_surface_is_exactly_the_curated_set() -> None:
    import math
    from itertools import pairwise

    from compute import metrics
    from compute.store import rw as _rw, pg as _pg
    from compute.endpoint import _NoopStore

    ns = build_namespace(make_contract(), Window(0, 1), _NoopStore())
    expected = (
        set(metrics.__all__)
        | {"metric", "bind", "window", "pl", "sql", "rw", "pg"}
        | {"prod", "pairwise", "sorted", "math"}
        | {"fetch_json"}
    )
    assert set(ns) == expected

    assert ns["prod"] is math.prod
    assert ns["pairwise"] is pairwise
    assert ns["sorted"] is sorted
    assert ns["math"] is math
    assert ns["pl"] is pl
    assert ns["window"] == Window(0, 1)
    assert ns["rw"] is _rw
    assert ns["pg"] is _pg
    # sql is a lambda wrapping the store — just verify it's callable
    assert callable(ns["sql"])
    for name in metrics.__all__:
        assert ns[name] is getattr(metrics, name)
    from compute.httpfetch import fetch_json as _fetch_json
    assert ns["fetch_json"] is _fetch_json


def test_source_can_call_injected_curated_names() -> None:
    from compute.endpoint import _NoopStore

    src = """
@metric(output="scalar")
def m():
    xs = sorted([3, 1, 2])
    factors = [1 + (b - a) for a, b in pairwise(xs)]
    return prod(factors) + math.floor(1.9) + len(xs)
"""
    contract = make_contract()
    ns = build_namespace(contract, Window(0, 1), _NoopStore())
    exec(src, ns)
    # prod([2, 2]) = 4 ; floor(1.9) = 1 ; len = 3 -> 8
    assert contract.registry.entrypoint() == 8


# ---------------------------------------------------------------------------
# _parse_body validation
# ---------------------------------------------------------------------------

def test_parse_body_missing_source_raises(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    from compute.endpoint import ComputeError
    with pytest.raises(ComputeError) as exc_info:
        endpoint.run_compute({"window": {"from": 0, "to": 1}}, "dsn", None)
    assert exc_info.value.status == 400
    assert "source" in exc_info.value.message


def test_parse_body_missing_window_raises(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    from compute.endpoint import ComputeError
    with pytest.raises(ComputeError) as exc_info:
        endpoint.run_compute({"source": "@metric(output='scalar')\ndef m(): return 1"}, "dsn", None)
    assert exc_info.value.status == 400
    assert "window" in exc_info.value.message


# ---------------------------------------------------------------------------
# _to_frame helpers (pure unit — no rwclient needed)
# ---------------------------------------------------------------------------

def test_to_frame_scalar_none_is_null():
    frame = _to_frame("scalar", None)
    assert frame["rows"] == [[None]]


def test_to_frame_series_dataframe():
    df = pl.DataFrame({"ts": [1, 2], "value": [10.0, 20.0]})
    frame = _to_frame("series", df)
    assert frame["columns"] == ["ts", "value"]
    assert frame["rows"] == [[1, 10.0], [2, 20.0]]


def test_to_frame_series_list_tuple_2():
    rows = [(1, 0.5), (2, 1.5)]
    frame = _to_frame("series", rows)
    assert frame["columns"] == ["ts", "value"]
    assert frame["rows"] == [[1, 0.5], [2, 1.5]]


def test_to_frame_series_empty_list():
    frame = _to_frame("series", [])
    assert frame["columns"] == []
    assert frame["rows"] == []


def test_to_frame_pl_series_with_nulls():
    frame = _to_frame("series", pl.Series("x", [1.0, None, 3.0]))
    assert frame["columns"] == ["x"]
    assert frame["rows"] == [[1.0], [None], [3.0]]


def test_to_frame_non_framable_type_raises():
    from compute.endpoint import ComputeError
    with pytest.raises(ComputeError) as exc_info:
        _to_frame("series", {"bad": "type"})
    assert "dict" in exc_info.value.message


def test_to_frame_list_of_non_tuple_raises():
    from compute.endpoint import ComputeError
    with pytest.raises(ComputeError) as exc_info:
        _to_frame("series", [1, 2, 3])
    assert "tuple" in exc_info.value.message


# ---------------------------------------------------------------------------
# NaN/Inf sanitization
# ---------------------------------------------------------------------------

def test_nan_inf_sanitized_to_null_in_scalar():
    import json

    for bad in (float("nan"), float("inf"), float("-inf")):
        frame = _to_frame("scalar", bad)
        assert frame["rows"] == [[None]], f"expected null for {bad!r}"
        raw = json.dumps(frame)
        assert "NaN" not in raw and "Infinity" not in raw


def test_nan_inf_sanitized_in_series_dataframe():
    import json

    df = pl.DataFrame({"ts": [1, 2, 3], "value": [0.1, float("nan"), float("inf")]})
    frame = _to_frame("series", df)
    assert frame["rows"][1][1] is None
    assert frame["rows"][2][1] is None
    raw = json.dumps(frame)
    assert "NaN" not in raw and "Infinity" not in raw


def test_nan_inf_sanitized_in_list_tuple():
    import json

    rows = [(1, 0.5), (2, float("nan")), (3, float("-inf"))]
    frame = _to_frame("series", rows)
    assert frame["rows"][1][1] is None
    assert frame["rows"][2][1] is None
    raw = json.dumps(frame)
    assert "NaN" not in raw and "Infinity" not in raw


# ---------------------------------------------------------------------------
# Server integration (health still works; full e2e via monkeypatched rwclient)
# ---------------------------------------------------------------------------

def test_health_still_works(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    srv, base = _serve_compute()
    try:
        with urllib.request.urlopen(base + "/health") as resp:
            assert resp.status == 200
            assert resp.read() == b"ok"
    finally:
        srv.shutdown()


class _E2EStore:
    """Fake Store for server e2e tests — bypasses rwclient entirely."""

    def __init__(self, frame: pl.DataFrame):
        self._frame = frame

    def run(self, spec):
        return self._frame

    def close(self):
        pass


def test_server_e2e_via_monkeypatched_rwclient(monkeypatch):
    """Full HTTP e2e: server -> run_compute -> fake Store."""
    _frame = pl.DataFrame({"ts": [1, 2], "nav": [10.0, 11.0]})
    monkeypatch.setattr(ep, "Store", lambda rw_dsn, pg_dsn: _E2EStore(_frame))
    srv, base = _serve_compute()
    source = (
        "@metric(output='series')\n"
        "def m():\n"
        "    df = sql('SELECT ts, nav FROM nav', )\n"
        "    return df\n"
    )
    try:
        status, body = _post_compute(
            base, {"source": source, "window": {"from": 0, "to": 9}}
        )
    finally:
        srv.shutdown()
    assert status == 200
    assert body["columns"] == ["ts", "nav"]


def test_server_e2e_json_type_fidelity(monkeypatch):
    """Deserialized JSON rows must contain native int/float, not strings."""
    _frame = pl.DataFrame({"ts": [1, 2], "nav": [10.0, 11.5]})
    monkeypatch.setattr(ep, "Store", lambda rw_dsn, pg_dsn: _E2EStore(_frame))
    srv, base = _serve_compute()
    source = (
        "@metric(output='series')\n"
        "def m():\n"
        "    df = sql('SELECT ts, nav FROM nav')\n"
        "    return df\n"
    )
    try:
        status, body = _post_compute(
            base, {"source": source, "window": {"from": 0, "to": 9}}
        )
    finally:
        srv.shutdown()
    assert status == 200
    rows = body["rows"]
    # ts column must be int, nav column must be float — no accidental stringification
    assert isinstance(rows[0][0], int), f"expected int for ts, got {type(rows[0][0])}"
    assert isinstance(rows[0][1], float), f"expected float for nav, got {type(rows[0][1])}"


def test_per_request_isolation(monkeypatch):
    """Two sequential /compute requests must not share contract registry state."""
    call_count = [0]
    frames = [
        pl.DataFrame({"ts": [1], "val": [100.0]}),
        pl.DataFrame({"ts": [2], "val": [200.0]}),
    ]

    class _CountingStore:
        def __init__(self, *a, **k):
            self._idx = call_count[0]
            call_count[0] += 1

        def run(self, spec):
            return frames[self._idx]

        def close(self):
            pass

    monkeypatch.setattr(ep, "Store", _CountingStore)
    srv, base = _serve_compute()

    source_a = (
        "@metric(output='series')\n"
        "def m():\n"
        "    return sql('SELECT ts, val FROM a')\n"
    )
    source_b = (
        "@metric(output='series')\n"
        "def m():\n"
        "    return sql('SELECT ts, val FROM b')\n"
    )
    try:
        status_a, body_a = _post_compute(
            base, {"source": source_a, "window": {"from": 0, "to": 9}}
        )
        status_b, body_b = _post_compute(
            base, {"source": source_b, "window": {"from": 0, "to": 9}}
        )
    finally:
        srv.shutdown()

    assert status_a == 200
    assert status_b == 200
    # Each request must return its own correct result — no registry leakage
    assert body_a["rows"] == [[1, 100.0]], f"request A got wrong rows: {body_a['rows']}"
    assert body_b["rows"] == [[2, 200.0]], f"request B got wrong rows: {body_b['rows']}"


def test_server_malformed_json_is_400(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    srv, base = _serve_compute()
    try:
        req = urllib.request.Request(
            base + "/compute", data=b"{not json", method="POST",
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req) as resp:
                status, body = resp.status, json.loads(resp.read())
        except urllib.error.HTTPError as exc:
            status, body = exc.code, json.loads(exc.read())
    finally:
        srv.shutdown()
    assert status == 400
    assert "error" in body


def test_server_zero_metric_is_400(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    srv, base = _serve_compute()
    src = "x = 1  # no @metric anywhere"
    try:
        status, body = _post_compute(
            base, {"source": src, "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
    assert status == 400
    assert "@metric" in body["error"]


def test_server_many_metric_is_400(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    srv, base = _serve_compute()
    src = """
@metric(output="scalar")
def a():
    return 1

@metric(output="scalar")
def b():
    return 2
"""
    try:
        status, body = _post_compute(
            base, {"source": src, "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
    assert status == 400
    assert "more than one @metric" in body["error"]


# ---------------------------------------------------------------------------
# POST /plan — still works (no gateway contact, no sql needed)
# ---------------------------------------------------------------------------

def _post_plan(base: str, body: dict) -> tuple[int, dict]:
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        base + "/plan", data=data, method="POST",
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def test_plan_returns_empty_bindings(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    srv, base = _serve_compute()
    src = """
@metric(output="scalar")
def m():
    return 0
"""
    try:
        status, body = _post_plan(base, {"source": src})
    finally:
        srv.shutdown()
    assert status == 200
    assert body == {"bindings": {}}


def test_plan_malformed_source_is_400(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    srv, base = _serve_compute()
    try:
        status, body = _post_plan(base, {"source": "raise ValueError('bad source')"})
    finally:
        srv.shutdown()
    assert status == 400
    assert "error" in body


def test_plan_missing_source_is_400(monkeypatch):
    monkeypatch.setattr(rwclient, "connect", lambda dsn: _FakeConn())
    monkeypatch.setattr(rwclient, "query", lambda conn, q, p=(): pl.DataFrame())
    srv, base = _serve_compute()
    try:
        status, body = _post_plan(base, {"not_source": "x"})
    finally:
        srv.shutdown()
    assert status == 400
    assert "source" in body["error"]
