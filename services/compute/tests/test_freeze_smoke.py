"""Smoke test for the frozen compute binary.

Marked ``freeze`` — excluded from the default fast unit run.  Run via:

    make compute-smoke          # builds first, then runs this
    pytest services/compute/tests/test_freeze_smoke.py -v -m freeze

Three tests are run against the frozen ``dist/compute`` binary:

1. **health + polars** — starts the binary, hits /health, then POSTs a
   /compute source that builds a polars DataFrame in-process (no @bind, no
   gateway required).  A correct neutral frame proves polars survives the
   freeze.

2. **plan / sqlglot import** — POSTs to /plan with a source that uses
   @metric.  The /plan handler exec's the source which imports
   ``compute.router`` (and therefore ``sqlglot``) at module init; a 200
   response proves sqlglot is present in the freeze.  Note: this exercises
   the *import*, not a dialect parse.

3. **sqlglot postgres-dialect parse** — POSTs to /compute with a source
   that has an @bind with an auto-routed SQL query.  The compute endpoint
   calls ``store.run(spec)`` which evaluates ``tables_in(spec.sql)``
   (``sqlglot.parse_one(sql, read="postgres")``) BEFORE attempting the
   catalog DB query.  Because no database is running, the request fails at
   the DB-connect step and returns a 400 with a connection/catalog error
   message.  The assertion checks that the error is NOT a sqlglot/dialect
   import error — proving that ``sqlglot.dialects.postgres`` (which is
   lazily loaded by ``parse_one``) was correctly bundled in the freeze.
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


_PLAN_SOURCE = """\
@metric(output="table")
def sqlglot_smoke():
    return pl.DataFrame({"ts": [1], "value": [1.0]})
"""


@pytest.mark.freeze
@pytest.mark.timeout(90)
def test_frozen_binary_plan_exercises_sqlglot() -> None:
    """POST /plan must succeed — proves sqlglot imports in the frozen binary.

    sqlglot is imported at binary startup when the server module loads
    ``compute.endpoint`` → ``compute.store`` → ``compute.router`` → ``sqlglot``.
    If sqlglot is missing from the freeze the binary would fail to start (health
    would never become ready).  This test provides a second checkpoint: a 200
    from /plan confirms the import chain is intact at request time as well.
    Note: this test proves the *import*, not a postgres-dialect *parse* — see
    ``test_frozen_binary_sqlglot_postgres_dialect_parse`` for that.
    """
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

        # /plan — triggers exec of source inside the frozen binary; compute.router
        # is imported transitively which in turn imports sqlglot.
        payload = json.dumps({"source": _PLAN_SOURCE}).encode()
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/plan",
            data=payload,
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(req) as resp:
            assert resp.status == 200
            result = json.loads(resp.read())

        assert "bindings" in result, f"unexpected /plan response: {result}"

    finally:
        proc.terminate()
        proc.wait(timeout=5)


# Source with an auto-routed @bind — triggers sqlglot.parse_one(sql, read="postgres")
# inside store.run() before any DB catalog query is attempted.
_DIALECT_SOURCE = """\
@bind(data="SELECT 1 AS n")
@metric(output="table")
def dialect_smoke(data):
    return data
"""

# Keywords that would appear in a sqlglot/dialect import error but NOT in a
# normal DB-connection failure.  If any appear in the response body the test
# fails — it means the dialect submodule was missing from the freeze.
_SQLGLOT_ERROR_MARKERS = ("sqlglot", "ModuleNotFound", "No module named", "dialect")


@pytest.mark.freeze
@pytest.mark.timeout(90)
def test_frozen_binary_sqlglot_postgres_dialect_parse() -> None:
    """Prove that sqlglot.parse_one(sql, read="postgres") works in the frozen binary.

    The @bind decorator registers a QuerySpec("auto", "SELECT 1 AS n", ()).
    When the compute endpoint runs ``store.run(spec)`` for that binding it calls
    ``decide_store(tables_in(spec.sql), self.catalog())``.  Python evaluates
    arguments left-to-right, so ``tables_in(spec.sql)`` — which calls
    ``sqlglot.parse_one(sql, read="postgres")`` and lazily loads
    ``sqlglot.dialects.postgres`` — runs BEFORE ``self.catalog()`` attempts
    the DB connection.

    With no database running, the request fails at the catalog/DB-connect step
    and the server returns a 400 JSON error whose message mentions the connection
    or catalog failure.  If the postgres dialect submodule were missing from the
    freeze the failure would instead be a ModuleNotFoundError at parse time, and
    the error message would contain "sqlglot", "ModuleNotFound", "No module named",
    or "dialect".

    The test therefore asserts:
    - The response is a JSON object with an "error" key (not a 5xx crash).
    - The error message does NOT contain any sqlglot-import marker string.
    """
    binary = os.path.abspath(_BINARY)
    assert os.path.isfile(binary), f"frozen binary not found: {binary!r} — run make compute-freeze first"

    port = _free_port()
    # Point the binary at unreachable DB ports so connection fails fast without
    # waiting for a timeout.  Port 1 is reserved and immediately refused on
    # most systems; this keeps the test quick.
    env = {
        **os.environ,
        "COMPUTE_PORT": str(port),
        "COMPUTE_HOST": "127.0.0.1",
        "RISINGWAVE_DSN": "postgres://root@127.0.0.1:1/dev",
        "POSTGRES_DSN": "postgres://postgres@127.0.0.1:1/control_db",
    }

    proc = subprocess.Popen(
        [binary],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    try:
        health_url = f"http://127.0.0.1:{port}/health"
        _wait_ready(health_url, timeout=30.0)

        # /compute with an auto-routed @bind — forces tables_in() (sqlglot parse)
        # before the catalog DB query.
        payload = json.dumps({
            "source": _DIALECT_SOURCE,
            "window": {"from": 0, "to": 999_999_999_999},
        }).encode()
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/compute",
            data=payload,
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req) as resp:
                # A 200 would mean the query somehow succeeded — unexpected but not wrong.
                body = json.loads(resp.read())
                assert False, f"expected a DB-connection error, got 200: {body}"
        except urllib.error.HTTPError as exc:
            # We expect a 400 (ComputeError from the binding) — anything 4xx is
            # acceptable; 5xx would mean an unhandled exception (e.g. store init
            # crash) which is also a failure mode worth distinguishing.
            raw = exc.read()
            assert exc.code < 500, (
                f"got HTTP {exc.code} (5xx = unhandled exception, likely store init crash "
                f"before parse ran); body: {raw[:300]}"
            )
            body = json.loads(raw)
            error_msg = body.get("error", "")
            for marker in _SQLGLOT_ERROR_MARKERS:
                assert marker.lower() not in error_msg.lower(), (
                    f"error message contains {marker!r} — sqlglot.dialects.postgres "
                    f"may be missing from the freeze.\nFull error: {error_msg!r}"
                )
            # Sanity: the error should mention something connection/catalog related.
            assert error_msg, "expected a non-empty error message in the 4xx response"

    finally:
        proc.terminate()
        proc.wait(timeout=5)
