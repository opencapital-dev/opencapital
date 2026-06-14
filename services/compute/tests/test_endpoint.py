"""``POST /compute`` endpoint tests — register -> fetch -> call -> frame.

Stands up (a) a stdlib stub read-gateway serving canned ``{columns, rows}`` for
``/v1/rows`` and (b) the real ``ComputeServer`` pointed at the stub.  Asserts the
neutral frame per output mode, the injected namespace surface, the error shapes
(author errors 400, gateway status propagated), per-request isolation, and that
health still works.  No live read-gateway.
"""

from __future__ import annotations

import json
import socket
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from compute.contract import Window, make_contract
from compute.endpoint import build_namespace
from compute.server import ComputeServer


# ---------------------------------------------------------------------------
# Stub read-gateway: maps selector -> canned {columns, rows}
# ---------------------------------------------------------------------------

def _make_gateway(rows_by_selector: dict, status: int = 200, error_body: bytes = b""):
    class _Stub(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(length))
            if status != 200:
                self._send(status, error_body)
                return
            selector = req["selector"]
            doc = rows_by_selector.get(selector, {"columns": ["ts"], "rows": []})
            self._send(200, json.dumps(doc).encode())

        def _send(self, code: int, body: bytes) -> None:
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a: object) -> None:
            pass

    return _Stub


def _serve_gateway(rows_by_selector: dict, status: int = 200, error_body: bytes = b""):
    server = ThreadingHTTPServer(
        ("127.0.0.1", 0), _make_gateway(rows_by_selector, status, error_body)
    )
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_address[1]}"


# ---------------------------------------------------------------------------
# Compute server harness pointed at a stub gateway
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
# Fixture sources
# ---------------------------------------------------------------------------

# series: equity curve via build_grid + cumulative_twr, framed as ts/value.
_SERIES_SRC = r"""
@bind(navs="nav{portfolio=\"$pid\"} @window", flows="flows{portfolio=\"$pid\"} @window")
@metric(output="series")
def equity_curve(navs, flows):
    t0, t1 = window
    grid = build_grid(t0, t1, "1d")
    flow_ts = flows["ts"].to_list() if flows.height else []
    pts = cumulative_twr(navs, flow_ts, t0, grid)
    return pl.DataFrame({"ts": [p[0] for p in pts], "value": [p[1] for p in pts]})
"""

# scalar: windowed TWR total return.
_SCALAR_SRC = r"""
@bind(navs="nav{portfolio=\"$pid\"} @window", flows="flows{portfolio=\"$pid\"} @window")
@metric(output="scalar")
def total_return(navs, flows):
    t0, t1 = window
    flow_ts = flows["ts"].to_list() if flows.height else []
    return twr(navs, flow_ts, t0, t1)
"""

# table: arbitrary multi-column frame straight from the bound rows.
_TABLE_SRC = r"""
@bind(navs="nav{portfolio=\"$pid\"} @window")
@metric(output="table")
def nav_table(navs):
    return navs.with_columns((pl.col("value") * 2).alias("doubled"))
"""


def _nav_doc():
    return {
        "columns": ["ts", "value"],
        "rows": [
            [0, 100.0],
            [86_400_000_000, 110.0],
            [2 * 86_400_000_000, 121.0],
        ],
    }


def _flows_doc():
    return {"columns": ["ts"], "rows": []}


# ---------------------------------------------------------------------------
# Per-mode end-to-end
# ---------------------------------------------------------------------------

def test_scalar_mode_end_to_end() -> None:
    gw, gw_url = _serve_gateway(
        {"nav{portfolio=\"$pid\"}": _nav_doc(), "flows{portfolio=\"$pid\"}": _flows_doc()}
    )
    srv, base = _serve_compute(gw_url)
    try:
        window = {"from": 0, "to": 2 * 86_400_000_000}
        status, body = _post_compute(
            base, {"source": _SCALAR_SRC, "jwt": "tok", "window": window}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

    assert status == 200
    assert body["output"] == "scalar"
    assert body["columns"] == ["value"]
    assert len(body["rows"]) == 1 and len(body["rows"][0]) == 1
    # NAV 100 -> 121 with no flows: total return = 0.21
    assert body["rows"][0][0] == pytest.approx(0.21)


def test_series_mode_end_to_end() -> None:
    gw, gw_url = _serve_gateway(
        {"nav{portfolio=\"$pid\"}": _nav_doc(), "flows{portfolio=\"$pid\"}": _flows_doc()}
    )
    srv, base = _serve_compute(gw_url)
    try:
        window = {"from": 0, "to": 2 * 86_400_000_000}
        status, body = _post_compute(
            base, {"source": _SERIES_SRC, "jwt": "tok", "window": window}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

    assert status == 200
    assert body["output"] == "series"
    assert body["columns"] == ["ts", "value"]
    # 3 daily grid points; first rebased at 0.0, last at 0.21
    assert [r[0] for r in body["rows"]] == [0, 86_400_000_000, 2 * 86_400_000_000]
    assert body["rows"][0][1] == pytest.approx(0.0)
    assert body["rows"][-1][1] == pytest.approx(0.21)


def test_table_mode_end_to_end() -> None:
    gw, gw_url = _serve_gateway({"nav{portfolio=\"$pid\"}": _nav_doc()})
    srv, base = _serve_compute(gw_url)
    try:
        window = {"from": 0, "to": 2 * 86_400_000_000}
        status, body = _post_compute(
            base, {"source": _TABLE_SRC, "jwt": "tok", "window": window}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

    assert status == 200
    assert body["output"] == "table"
    assert body["columns"] == ["ts", "value", "doubled"]
    assert body["rows"] == [
        [0, 100.0, 200.0],
        [86_400_000_000, 110.0, 220.0],
        [2 * 86_400_000_000, 121.0, 242.0],
    ]


def test_scalar_none_serialises_as_json_null() -> None:
    # Empty NAV rows -> twr returns None -> value null.
    gw, gw_url = _serve_gateway(
        {
            "nav{portfolio=\"$pid\"}": {"columns": ["ts", "value"], "rows": []},
            "flows{portfolio=\"$pid\"}": _flows_doc(),
        }
    )
    srv, base = _serve_compute(gw_url)
    try:
        window = {"from": 0, "to": 2 * 86_400_000_000}
        status, body = _post_compute(
            base, {"source": _SCALAR_SRC, "jwt": "tok", "window": window}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

    assert status == 200
    assert body["output"] == "scalar"
    assert body["rows"] == [[None]]


def test_int_and_float_types_preserved() -> None:
    gw, gw_url = _serve_gateway({"nav{portfolio=\"$pid\"}": _nav_doc()})
    srv, base = _serve_compute(gw_url)
    try:
        window = {"from": 0, "to": 2 * 86_400_000_000}
        _, body = _post_compute(
            base, {"source": _TABLE_SRC, "jwt": "tok", "window": window}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

    # ts stays int, nav/doubled stay float (not stringified).
    assert isinstance(body["rows"][0][0], int)
    assert isinstance(body["rows"][0][1], float)
    assert isinstance(body["rows"][0][2], float)


# ---------------------------------------------------------------------------
# Namespace surface (definition of provided surface, not a lockdown)
# ---------------------------------------------------------------------------

def test_namespace_surface_is_exactly_the_curated_set() -> None:
    import math
    from itertools import pairwise

    from compute import metrics

    ns = build_namespace(make_contract(), Window(0, 1))
    expected = (
        set(metrics.__all__)
        | {"bind", "metric", "window", "pl"}
        | {"prod", "pairwise", "sorted", "math"}
    )
    assert set(ns) == expected

    # The injected names resolve to the right objects.
    assert ns["prod"] is math.prod
    assert ns["pairwise"] is pairwise
    assert ns["sorted"] is sorted
    assert ns["math"] is math
    import polars as pl
    assert ns["pl"] is pl
    assert ns["window"] == Window(0, 1)
    for name in metrics.__all__:
        assert ns[name] is getattr(metrics, name)


def test_source_can_call_injected_curated_names() -> None:
    # Plain exec: the source calls prod/pairwise/sorted/math AND normal builtins.
    src = """
@metric(output="scalar")
def m():
    xs = sorted([3, 1, 2])
    factors = [1 + (b - a) for a, b in pairwise(xs)]
    return prod(factors) + math.floor(1.9) + len(xs)
"""
    contract = make_contract()
    ns = build_namespace(contract, Window(0, 1))
    exec(src, ns)
    # prod([2, 2]) = 4 ; floor(1.9) = 1 ; len = 3 -> 8
    assert contract.registry.entrypoint() == 8


# ---------------------------------------------------------------------------
# Error cases
# ---------------------------------------------------------------------------

def test_source_that_raises_is_400() -> None:
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    src = "raise ValueError('boom from author')"
    try:
        status, body = _post_compute(
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "boom from author" in body["error"]


def test_entrypoint_that_raises_is_400() -> None:
    gw, gw_url = _serve_gateway({"nav{portfolio=\"$pid\"}": _nav_doc()})
    srv, base = _serve_compute(gw_url)
    src = r"""
@bind(navs="nav{portfolio=\"$pid\"} @window")
@metric(output="scalar")
def m(navs):
    raise RuntimeError("explode in call")
"""
    try:
        status, body = _post_compute(
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "explode in call" in body["error"]


def test_zero_metric_is_400() -> None:
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    src = "x = 1  # no @metric anywhere"
    try:
        status, body = _post_compute(
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "@metric" in body["error"]


def test_many_metric_is_400() -> None:
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
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
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "more than one @metric" in body["error"]


def test_gateway_401_is_surfaced() -> None:
    gw, gw_url = _serve_gateway(
        {}, status=401, error_body=b'{"error":"forbidden org scope"}'
    )
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_compute(
            base, {"source": _SCALAR_SRC, "jwt": "bad", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 401
    assert "forbidden org scope" in body["error"]


def test_malformed_json_is_400() -> None:
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
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
        gw.shutdown()
    assert status == 400
    assert "error" in body


def test_missing_fields_is_400() -> None:
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_compute(base, {"jwt": "t", "window": {"from": 0, "to": 1}})
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "source" in body["error"]


# ---------------------------------------------------------------------------
# Per-request isolation
# ---------------------------------------------------------------------------

def test_per_request_isolation() -> None:
    # Two requests through the same server: the first registers a metric, the
    # second (a different source) must see a fresh contract — no leakage.
    gw, gw_url = _serve_gateway({"nav{portfolio=\"$pid\"}": _nav_doc()})
    srv, base = _serve_compute(gw_url)
    try:
        s1, b1 = _post_compute(
            base, {"source": _TABLE_SRC, "jwt": "t", "window": {"from": 0, "to": 2 * 86_400_000_000}}
        )
        # Second source with a DIFFERENT entrypoint/output; if state leaked the
        # registry would already hold an entrypoint and raise "more than one".
        s2, b2 = _post_compute(
            base, {"source": _TABLE_SRC, "jwt": "t", "window": {"from": 0, "to": 2 * 86_400_000_000}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert s1 == 200 and s2 == 200
    assert b1["output"] == "table" and b2["output"] == "table"


# ---------------------------------------------------------------------------
# Health still works
# ---------------------------------------------------------------------------

def test_health_still_works() -> None:
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    try:
        with urllib.request.urlopen(base + "/health") as resp:
            assert resp.status == 200
            assert resp.read() == b"ok"
    finally:
        srv.shutdown()
        gw.shutdown()


# ---------------------------------------------------------------------------
# _to_frame extension: list[tuple], pl.Series, and the raw-cumulative_twr path
# ---------------------------------------------------------------------------

# Panel that returns cumulative_twr(...) directly — a list[tuple[int, float|None]].
# No manual pl.DataFrame wrapping. This proves list[tuple] framing end-to-end.
_SERIES_RAW_LIST_SRC = r"""
@bind(navs="nav{portfolio=\"$pid\"} @window", flows="flows{portfolio=\"$pid\"} @window")
@metric(output="series")
def equity_curve_raw(navs, flows):
    t0, t1 = window
    grid = build_grid(t0, t1, "1d")
    flow_ts = flows["ts"].to_list() if flows.height else []
    return cumulative_twr(navs, flow_ts, t0, grid)
"""

# Panel returning a pl.Series (a single column of nav values).
_SERIES_POLARS_SERIES_SRC = r"""
@bind(navs="nav{portfolio=\"$pid\"} @window")
@metric(output="series")
def nav_column(navs):
    return navs["value"].alias("value")
"""


def test_series_raw_list_tuple_end_to_end() -> None:
    """A panel can return cumulative_twr(...) directly; list[tuple] is framed correctly."""
    gw, gw_url = _serve_gateway(
        {"nav{portfolio=\"$pid\"}": _nav_doc(), "flows{portfolio=\"$pid\"}": _flows_doc()}
    )
    srv, base = _serve_compute(gw_url)
    try:
        window = {"from": 0, "to": 2 * 86_400_000_000}
        status, body = _post_compute(
            base, {"source": _SERIES_RAW_LIST_SRC, "jwt": "tok", "window": window}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

    assert status == 200
    assert body["output"] == "series"
    assert body["columns"] == ["ts", "value"]
    assert [r[0] for r in body["rows"]] == [0, 86_400_000_000, 2 * 86_400_000_000]
    assert body["rows"][0][1] == pytest.approx(0.0)
    assert body["rows"][-1][1] == pytest.approx(0.21)


def test_series_polars_series_end_to_end() -> None:
    """A panel returning a pl.Series is framed with the series name as the column."""
    gw, gw_url = _serve_gateway({"nav{portfolio=\"$pid\"}": _nav_doc()})
    srv, base = _serve_compute(gw_url)
    try:
        window = {"from": 0, "to": 2 * 86_400_000_000}
        status, body = _post_compute(
            base, {"source": _SERIES_POLARS_SERIES_SRC, "jwt": "tok", "window": window}
        )
    finally:
        srv.shutdown()
        gw.shutdown()

    assert status == 200
    assert body["output"] == "series"
    assert body["columns"] == ["value"]
    assert body["rows"] == [[100.0], [110.0], [121.0]]


def test_series_pl_series_nulls_preserved() -> None:
    """A pl.Series with null cells frames with JSON-null values preserved."""
    import polars as pl

    from compute.endpoint import _to_frame

    frame = _to_frame("series", pl.Series("x", [1.0, None, 3.0]))
    assert frame["columns"] == ["x"]
    assert frame["rows"] == [[1.0], [None], [3.0]]


def test_series_non_framable_type_is_400() -> None:
    """A panel returning an unrecognised type for series output yields a 400."""
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    src = """
@metric(output="series")
def bad():
    return {"this": "is not a valid series type"}
"""
    try:
        status, body = _post_compute(
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "dict" in body["error"]


def test_series_list_of_non_tuple_is_400() -> None:
    """A panel returning list[non-tuple] for series output yields a 400."""
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    src = """
@metric(output="series")
def bad():
    return [1, 2, 3]
"""
    try:
        status, body = _post_compute(
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "tuple" in body["error"]


def test_series_empty_list_frames_to_empty() -> None:
    """An empty list return frames as empty columns/rows without error."""
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    src = """
@metric(output="series")
def empty():
    return []
"""
    try:
        status, body = _post_compute(
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 200
    assert body["output"] == "series"
    assert body["columns"] == []
    assert body["rows"] == []


# ---------------------------------------------------------------------------
# NaN/Inf sanitization (Fix 1)
# ---------------------------------------------------------------------------

def test_nan_inf_sanitized_to_null_in_scalar() -> None:
    """scalar NaN and Inf become JSON null; raw body contains no NaN/Infinity tokens."""
    from compute.endpoint import _to_frame
    import json

    for bad in (float("nan"), float("inf"), float("-inf")):
        frame = _to_frame("scalar", bad)
        assert frame["rows"] == [[None]], f"expected null for {bad!r}"
        raw = json.dumps(frame)
        assert "NaN" not in raw and "Infinity" not in raw, f"non-finite token in {raw!r}"


def test_nan_inf_sanitized_to_null_in_series_dataframe() -> None:
    """NaN/Inf cells in a pl.DataFrame series output become JSON null."""
    import json
    import polars as pl
    from compute.endpoint import _to_frame

    df = pl.DataFrame({"ts": [1, 2, 3], "value": [0.1, float("nan"), float("inf")]})
    frame = _to_frame("series", df)
    assert frame["rows"][1][1] is None, "NaN cell should be null"
    assert frame["rows"][2][1] is None, "Inf cell should be null"
    raw = json.dumps(frame)
    assert "NaN" not in raw and "Infinity" not in raw


def test_nan_inf_sanitized_in_list_tuple_series() -> None:
    """NaN/Inf in list[tuple] series rows become JSON null."""
    import json
    from compute.endpoint import _to_frame

    rows = [(1, 0.5), (2, float("nan")), (3, float("-inf"))]
    frame = _to_frame("series", rows)
    assert frame["rows"][1][1] is None
    assert frame["rows"][2][1] is None
    raw = json.dumps(frame)
    assert "NaN" not in raw and "Infinity" not in raw


def test_nan_inf_sanitized_end_to_end() -> None:
    """A metric returning NaN yields a parseable JSON response with null, not bare NaN."""
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    src = """
@metric(output="scalar")
def nan_metric():
    return float("nan")
"""
    try:
        status, body = _post_compute(
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 200
    assert body["output"] == "scalar"
    assert body["rows"] == [[None]]


def test_inf_sanitized_end_to_end() -> None:
    """A metric returning Inf yields a parseable JSON response with null, not bare Infinity."""
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    src = """
@metric(output="scalar")
def inf_metric():
    return float("inf")
"""
    try:
        status, body = _post_compute(
            base, {"source": src, "jwt": "t", "window": {"from": 0, "to": 1}}
        )
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 200
    assert body["output"] == "scalar"
    assert body["rows"] == [[None]]


# ---------------------------------------------------------------------------
# prefetched bindings: plugin-local SQLite rows bypass read-gateway
# ---------------------------------------------------------------------------

# Source binding two params: a (nav) fetched from gateway, b (flows) prefetched.
_PREFETCH_SRC = r"""
@bind(a="nav{portfolio=\"$pid\"} @window", b="flows{portfolio=\"$pid\"} @window")
@metric(output="table")
def both_frames(a, b):
    return pl.DataFrame({"nav_height": [a.height], "flow_height": [b.height]})
"""


def test_prefetched_binding_skips_gateway() -> None:
    """A prefetched binding is used directly; only the unprefetched one hits the gateway."""
    gw_hits: list[str] = []

    class _TrackingHandler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(length))
            gw_hits.append(req["selector"])
            doc = {"columns": ["ts", "value"], "rows": [[0, 100.0], [86_400_000_000, 110.0]]}
            body = json.dumps(doc).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a: object) -> None:
            pass

    gw_server = ThreadingHTTPServer(("127.0.0.1", 0), _TrackingHandler)
    threading.Thread(target=gw_server.serve_forever, daemon=True).start()
    gw_url = f"http://127.0.0.1:{gw_server.server_address[1]}"

    srv, base = _serve_compute(gw_url)
    # b is prefetched (3 flow rows); only a should hit the gateway.
    prefetched_b = {
        "columns": ["ts"],
        "rows": [[1_000_000], [2_000_000], [3_000_000]],
    }
    try:
        window = {"from": 0, "to": 2 * 86_400_000_000}
        status, body = _post_compute(
            base,
            {
                "source": _PREFETCH_SRC,
                "jwt": "tok",
                "window": window,
                "prefetched": {"b": prefetched_b},
            },
        )
    finally:
        srv.shutdown()
        gw_server.shutdown()

    assert status == 200
    assert body["output"] == "table"
    assert body["columns"] == ["nav_height", "flow_height"]
    # a has 2 rows (from gateway), b has 3 rows (from prefetched).
    assert body["rows"] == [[2, 3]]
    # gateway was hit exactly once, for selector a — not for b.
    assert gw_hits == ["nav{portfolio=\"$pid\"}"]


# ---------------------------------------------------------------------------
# POST /plan — binding discovery without data fetch
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


# Source with two bindings: one plugin-prefixed @latest, one plain @window.
_PLAN_SRC = r"""
@bind(
    b="yfinance-app/classification{portfolio=\"$pid\"} @latest",
    a="nav{portfolio=\"$pid\"} @window",
)
@metric(output="scalar")
def m(a, b):
    return 0
"""


def test_plan_returns_raw_selectors() -> None:
    """POST /plan returns raw selectors — prefix and @mode preserved — without gateway contact."""
    gw_hits: list[str] = []

    class _TrackingHandler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            self.rfile.read(length)
            gw_hits.append("hit")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"{}")

        def log_message(self, *a: object) -> None:
            pass

    gw_server = ThreadingHTTPServer(("127.0.0.1", 0), _TrackingHandler)
    threading.Thread(target=gw_server.serve_forever, daemon=True).start()
    gw_url = f"http://127.0.0.1:{gw_server.server_address[1]}"

    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_plan(base, {"source": _PLAN_SRC})
    finally:
        srv.shutdown()
        gw_server.shutdown()

    assert status == 200
    assert body == {
        "bindings": {
            "a": r'nav{portfolio="$pid"} @window',
            "b": r'yfinance-app/classification{portfolio="$pid"} @latest',
        }
    }
    assert gw_hits == [], "gateway must not be contacted by /plan"


def test_plan_malformed_source_is_400() -> None:
    """A source that raises during exec yields 400 from /plan."""
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_plan(base, {"source": "raise ValueError('bad source')"})
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "error" in body


def test_plan_missing_source_is_400() -> None:
    """Missing 'source' field yields 400 from /plan."""
    gw, gw_url = _serve_gateway({})
    srv, base = _serve_compute(gw_url)
    try:
        status, body = _post_plan(base, {"not_source": "x"})
    finally:
        srv.shutdown()
        gw.shutdown()
    assert status == 400
    assert "source" in body["error"]
