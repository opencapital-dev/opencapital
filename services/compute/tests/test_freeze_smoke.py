"""Smoke test for the frozen compute binary.

Marked ``freeze`` — excluded from the default fast unit run.  Run via:

    make compute-smoke          # builds first, then runs this
    pytest services/compute/tests/test_freeze_smoke.py -v -m freeze

The test starts the binary on a free loopback port, hits /health, then
issues a /compute call whose source builds a polars DataFrame in-process
(no @bind, no gateway required).  A correct neutral frame proves polars
survives the freeze.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import time
import urllib.request

import pytest

_BINARY = os.path.join(
    os.path.dirname(__file__), "..", "dist", "compute"
)

_SOURCE = """\
@metric(output="series")
def polars_smoke():
    df = pl.DataFrame({"ts": [1_000_000, 2_000_000], "value": [1.0, 2.0]})
    return df
"""


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait_ready(url: str, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            urllib.request.urlopen(url, timeout=0.5)
            return
        except Exception:
            time.sleep(0.1)
    raise TimeoutError(f"frozen binary did not become ready at {url} within {timeout}s")


@pytest.mark.freeze
@pytest.mark.timeout(60)
def test_frozen_binary_health_and_polars_compute() -> None:
    binary = os.path.abspath(_BINARY)
    assert os.path.isfile(binary), f"frozen binary not found: {binary!r} — run make compute-freeze first"

    port = _free_port()
    env = {**os.environ, "COMPUTE_PORT": str(port), "COMPUTE_HOST": "127.0.0.1"}

    proc = subprocess.Popen(
        [binary],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    try:
        health_url = f"http://127.0.0.1:{port}/health"
        _wait_ready(health_url, timeout=30.0)

        # /health
        with urllib.request.urlopen(health_url) as resp:
            assert resp.status == 200
            assert resp.read() == b"ok"

        # /compute — source uses pl.DataFrame directly (no @bind, no gateway)
        payload = json.dumps({
            "source": _SOURCE,
            "jwt": "smoke-test-token",
            "window": {"from": 0, "to": 999_999_999_999},
        }).encode()
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/compute",
            data=payload,
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(req) as resp:
            assert resp.status == 200
            frame = json.loads(resp.read())

        assert frame["output"] == "series"
        assert frame["columns"] == ["ts", "value"]
        assert frame["rows"] == [[1_000_000, 1.0], [2_000_000, 2.0]]

    finally:
        proc.terminate()
        proc.wait(timeout=5)
