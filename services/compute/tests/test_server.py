"""Health endpoint tests for the compute service HTTP server."""

from __future__ import annotations

import socket
import threading
import time
import urllib.request
from http import HTTPStatus

import pytest

from compute.server import ComputeServer


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
    raise TimeoutError(f"server did not become ready at {url} within {timeout}s")


def test_health_returns_ok() -> None:
    port = _free_port()
    server = ComputeServer(host="127.0.0.1", port=port)
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    url = f"http://127.0.0.1:{port}/health"
    _wait_ready(url)
    with urllib.request.urlopen(url) as resp:
        assert resp.status == HTTPStatus.OK
        body = resp.read()
    assert body == b"ok"
    server.shutdown()


def test_binds_loopback_by_default() -> None:
    port = _free_port()
    server = ComputeServer(port=port)
    assert server.server_address[0] == "127.0.0.1"
    server.server_close()


def test_env_driven_bind(monkeypatch: pytest.MonkeyPatch) -> None:
    port = _free_port()
    monkeypatch.setenv("COMPUTE_HOST", "127.0.0.1")
    monkeypatch.setenv("COMPUTE_PORT", str(port))
    server = ComputeServer.from_env()
    assert server.server_address == ("127.0.0.1", port)
    server.server_close()
