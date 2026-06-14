"""Read-gateway rows-client tests.

Stands up a stdlib stub HTTP server returning a canned ``{columns, rows}``
body (T1 server-test pattern) and asserts the client builds the right polars
DataFrame, sends the wire-contract request shape, and surfaces non-200 errors.
No real read-gateway involved.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import polars as pl
import pytest

from compute.contract import Binding, Window
from compute.gateway import GatewayError, fetch_rows

# Captured request state, populated by the stub handler.
_captured: dict = {}


def _make_handler(status: int, body: bytes):
    class _Stub(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length)
            _captured["path"] = self.path
            _captured["auth"] = self.headers.get("Authorization")
            _captured["content_type"] = self.headers.get("Content-Type")
            _captured["body"] = json.loads(raw) if raw else None
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args: object) -> None:
            pass

    return _Stub


def _serve(status: int, body: bytes):
    server = ThreadingHTTPServer(("127.0.0.1", 0), _make_handler(status, body))
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    base_url = f"http://127.0.0.1:{server.server_address[1]}"
    return server, base_url


def setup_function() -> None:
    _captured.clear()


def test_builds_dataframe_with_ts_int64_and_nulls() -> None:
    body = json.dumps(
        {
            "columns": ["ts", "nav", "flows"],
            "rows": [
                [1_000, 100.5, None],
                [2_000, None, 5.0],
                [3_000, 102.0, 0.0],
            ],
        }
    ).encode()
    server, base_url = _serve(200, body)
    try:
        binding = Binding("nav{portfolio=\"$pid\"}", "window")
        df = fetch_rows(base_url, "tok", binding, Window(1_000, 3_000))
    finally:
        server.shutdown()

    assert df.columns == ["ts", "nav", "flows"]
    assert df.schema["ts"] == pl.Int64
    assert df.schema["nav"] == pl.Float64
    assert df.schema["flows"] == pl.Float64
    assert df["ts"].to_list() == [1_000, 2_000, 3_000]
    assert df["nav"].to_list() == [100.5, None, 102.0]
    assert df["flows"].to_list() == [None, 5.0, 0.0]
    # null preserved as a real null, not a stringified object
    assert df["nav"].null_count() == 1


def test_sends_wire_contract_request() -> None:
    body = json.dumps({"columns": ["ts"], "rows": [[1]]}).encode()
    server, base_url = _serve(200, body)
    try:
        binding = Binding("flows{portfolio=\"$pid\"}", "asof")
        fetch_rows(base_url, "secret-jwt", binding, Window(111, 222))
    finally:
        server.shutdown()

    assert _captured["path"] == "/v1/rows"
    assert _captured["auth"] == "Bearer secret-jwt"
    assert _captured["content_type"] == "application/json"
    assert _captured["body"] == {
        "selector": "flows{portfolio=\"$pid\"}",
        "mode": "asof",
        "from": 111,
        "to": 222,
    }


def test_mode_none_serialised_as_null() -> None:
    body = json.dumps({"columns": ["ts"], "rows": [[1]]}).encode()
    server, base_url = _serve(200, body)
    try:
        fetch_rows(base_url, "t", Binding("x{portfolio=\"$pid\"}", None), Window(1, 2))
    finally:
        server.shutdown()
    assert _captured["body"]["mode"] is None


def test_empty_rows_yields_typed_empty_frame() -> None:
    body = json.dumps({"columns": ["ts", "nav"], "rows": []}).encode()
    server, base_url = _serve(200, body)
    try:
        df = fetch_rows(base_url, "t", Binding("x{portfolio=\"$pid\"}", "window"), Window(1, 2))
    finally:
        server.shutdown()
    assert df.columns == ["ts", "nav"]
    assert df.height == 0
    assert df.schema["ts"] == pl.Int64


def test_401_raises_gateway_error() -> None:
    body = b'{"error":"forbidden org scope"}'
    server, base_url = _serve(401, body)
    try:
        with pytest.raises(GatewayError) as exc:
            fetch_rows(base_url, "bad", Binding("x{portfolio=\"$pid\"}", "asof"), Window(1, 2))
    finally:
        server.shutdown()
    assert exc.value.status == 401
    assert "forbidden org scope" in exc.value.body


def test_500_raises_gateway_error() -> None:
    server, base_url = _serve(500, b"boom")
    try:
        with pytest.raises(GatewayError) as exc:
            fetch_rows(base_url, "t", Binding("x{portfolio=\"$pid\"}", "window"), Window(1, 2))
    finally:
        server.shutdown()
    assert exc.value.status == 500
    assert "boom" in exc.value.body
