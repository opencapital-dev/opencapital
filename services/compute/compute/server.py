"""Threaded HTTP server for the compute sidecar.

Bind address and port are read from environment variables:
  COMPUTE_HOST       — defaults to 127.0.0.1 (loopback only)
  COMPUTE_PORT       — defaults to 8790
  RISINGWAVE_DSN     — RisingWave pgwire DSN the /compute endpoint connects to

Routes:
  GET  /health   →  200 "ok"
  POST /compute  →  200 neutral frame {"output","columns","rows"} (see endpoint)
  POST /plan     →  200 {"bindings": {}}
"""

from __future__ import annotations

import json
import logging
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from compute.endpoint import ComputeError, run_compute, run_plan

log = logging.getLogger("compute.server")

_DEFAULT_HOST = "127.0.0.1"
_DEFAULT_PORT = 8790
_DEFAULT_DSN = "postgres://root@127.0.0.1:4566/dev?sslmode=disable"


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        if self.path == "/health":
            self._send_text(200, b"ok")
        else:
            self.send_error(404)

    def do_POST(self) -> None:
        if self.path not in ("/compute", "/plan"):
            self.send_error(404)
            return
        try:
            body = json.loads(self._read_body())
        except (ValueError, UnicodeDecodeError) as exc:
            log.warning("compute: malformed request body: %s", exc)
            self._send_json(400, {"error": f"invalid JSON body: {exc}"})
            return
        if self.path == "/plan":
            try:
                result = run_plan(body)
            except ComputeError as exc:
                log.warning("plan: %s", exc.message)
                self._send_json(exc.status, {"error": exc.message})
                return
            self._send_json(200, result)
            return
        try:
            result = run_compute(body, self.server.dsn)
        except ComputeError as exc:
            log.warning("compute: %s", exc.message)
            self._send_json(exc.status, {"error": exc.message})
            return
        self._send_json(200, result)

    def _read_body(self) -> bytes:
        length = int(self.headers.get("Content-Length", "0"))
        return self.rfile.read(length)

    def _send_text(self, status: int, body: bytes) -> None:
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, status: int, doc: dict) -> None:
        body = json.dumps(doc).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt: str, *args: object) -> None:
        log.debug(fmt, *args)


class ComputeServer(ThreadingHTTPServer):
    """Loopback-only threaded HTTP server for the compute sidecar."""

    def __init__(
        self,
        host: str = _DEFAULT_HOST,
        port: int = _DEFAULT_PORT,
        dsn: str = _DEFAULT_DSN,
    ) -> None:
        super().__init__((host, port), _Handler)
        self.dsn = dsn
        log.info(
            "compute server listening host=%s port=%d dsn=%s",
            host, port, dsn,
        )

    @classmethod
    def from_env(cls) -> ComputeServer:
        host = os.environ.get("COMPUTE_HOST", _DEFAULT_HOST)
        port = int(os.environ.get("COMPUTE_PORT", _DEFAULT_PORT))
        dsn = os.environ.get("RISINGWAVE_DSN", _DEFAULT_DSN)
        return cls(host=host, port=port, dsn=dsn)


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    log.info("compute server starting")
    server = ComputeServer.from_env()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        log.info("compute server stopped")


if __name__ == "__main__":
    main()
