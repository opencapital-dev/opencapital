"""Health endpoint tests for the compute service HTTP server."""

from __future__ import annotations

import json
import socket
import threading
import time
import urllib.error
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


def test_unserializable_result_returns_500_not_dropped_connection() -> None:
    # A metric returning a non-JSON-serializable cell (datetime) must surface as a
    # visible 500, not a dropped connection (which reaches the caller as EOF).
    port = _free_port()
    server = ComputeServer(
        host="127.0.0.1", port=port,
        dsn="postgres://x@127.0.0.1:1/x", pg_dsn="postgres://x@127.0.0.1:1/x",
    )
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{port}"
    _wait_ready(base + "/health")
    src = (
        '@metric(output="series")\n'
        "def m():\n"
        "    import datetime\n"
        '    return pl.DataFrame({"ts": [datetime.datetime(2024, 1, 1)], "value": [1.0]})\n'
    )
    body = json.dumps({"source": src, "window": {"from": 0, "to": 1}}).encode()
    req = urllib.request.Request(
        base + "/compute", data=body,
        headers={"Content-Type": "application/json"}, method="POST",
    )
    try:
        code = urllib.request.urlopen(req, timeout=5).status
    except urllib.error.HTTPError as exc:
        code = exc.code  # a 500 is raised as HTTPError
    finally:
        server.shutdown()
    assert code == 500
