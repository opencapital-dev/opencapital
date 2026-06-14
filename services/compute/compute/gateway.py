"""Read-gateway rows client — fetch one binding's org-scoped rows as a polars frame.

Stdlib HTTP only (``urllib.request``) to keep the compute service framework-free
and freeze-friendly: no ``requests`` / ``httpx``.

Wire contract (the Go read-gateway in T8 implements exactly this)
-----------------------------------------------------------------
Request : ``POST {base_url}/v1/rows`` with ``Authorization: Bearer <jwt>`` and
          JSON body ``{"selector", "mode", "from", "to"}`` where ``mode`` is the
          binding's mode or ``null`` and ``from`` / ``to`` are int-microseconds.
Response: 200 JSON ``{"columns": [...], "rows": [[...], ...]}`` — ``rows`` are
          arrays in ``columns`` order; ``ts`` values are int-microseconds and
          numeric values may be JSON ``null``.
Non-200 : raised as ``GatewayError`` carrying the status + body (401/403 scoping
          and auth failures surface faithfully).
"""

from __future__ import annotations

import json
import logging
import urllib.error
import urllib.request

import polars as pl

from compute.contract import Binding, Window

log = logging.getLogger("compute.gateway")

_ROWS_PATH = "/v1/rows"
_TIMEOUT_S = 30.0


class GatewayError(Exception):
    """A non-200 response from the read-gateway, carrying its status and body."""

    def __init__(self, status: int, body: str) -> None:
        super().__init__(f"read-gateway returned {status}: {body}")
        self.status = status
        self.body = body


def fetch_rows(
    base_url: str,
    jwt: str,
    binding: Binding,
    window: Window,
) -> pl.DataFrame:
    """Fetch *binding*'s rows over *window* and return them as a polars frame.

    POSTs the wire-contract request to ``{base_url}/v1/rows`` with a Bearer
    *jwt*, then materialises the ``{columns, rows}`` response.  A ``ts`` column,
    if present, is coerced to ``Int64``; other columns infer their dtype with
    JSON ``null`` preserved as a real null.  Raises ``GatewayError`` on any
    non-200 response.
    """
    payload = json.dumps(
        {
            "selector": binding.selector,
            "mode": binding.mode,
            "from": window.t0,
            "to": window.t1,
        }
    ).encode()
    req = urllib.request.Request(
        base_url.rstrip("/") + _ROWS_PATH,
        data=payload,
        method="POST",
        headers={
            "Authorization": f"Bearer {jwt}",
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=_TIMEOUT_S) as resp:
            body = resp.read()
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", "replace")
        log.warning("read-gateway error status=%d selector=%s", exc.code, binding.selector)
        raise GatewayError(exc.code, detail) from exc

    doc = json.loads(body)
    return _frame_from(doc["columns"], doc["rows"])


def _frame_from(columns: list[str], rows: list[list]) -> pl.DataFrame:
    """Build a polars frame from column names + row arrays in column order.

    Transposes ``rows`` into per-column lists so polars infers each column's
    dtype independently — a column of mixed number/None becomes ``Float64`` with
    nulls, never an object column.  ``ts`` is forced to ``Int64``.  An empty
    ``rows`` yields a typed, zero-height frame (``ts`` Int64, others Null).
    """
    # Go's JSON serializes whole-number floats without a decimal point, so a
    # numeric column round-trips as mixed int/float, which polars' STRICT
    # construction rejects in either direction. Build each column non-strictly
    # so polars coerces mixed int/float to a common dtype. `ts` is genuine
    # int-µs; an empty column for it stays Int64 (not Null).
    series = []
    for i, name in enumerate(columns):
        vals = [row[i] for row in rows]
        dtype = pl.Int64 if name == "ts" else None
        series.append(pl.Series(name, vals, dtype=dtype, strict=False))
    return pl.DataFrame(series)
