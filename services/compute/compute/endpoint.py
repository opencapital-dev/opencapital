"""``POST /compute`` — run one panel source register -> call -> frame.

The HTTP layer (``compute.server``) delegates here.  Given a request body
``{"source", "window": {"from", "to"}}`` this module:

  1. builds a fresh, isolated ``Contract`` (no cross-request leakage) and the
     exec namespace — the metric module's panel-facing names, ``metric`` /
     ``window`` / ``pl`` / ``sql``, plus a curated set of stdlib names the
     formulas use;
  2. ``exec``s the source so the decorator registers the entrypoint and the
     output mode, then asserts the contract is complete;
  3. calls the entrypoint with no arguments; the entrypoint calls ``sql()``
     — injected into the namespace — to pull its own data from RisingWave;
  4. maps the return to the NEUTRAL FRAME the P2 plugin consumes:
     ``{"output", "columns", "rows"}``.

Execution is plain ``exec`` — UNRESTRICTED.  This is single-tenant by
deployment, so the namespace is a *definition of the provided surface*, not a
sandbox.  No restricted ``__builtins__``, no pretend boundary.
"""

from __future__ import annotations

import logging
import math
from itertools import pairwise

import polars as pl

from compute import metrics, rwclient
from compute.contract import ContractError, Window, make_contract

log = logging.getLogger("compute.endpoint")

# The curated stdlib surface a panel source may call, beyond Python's own
# builtins (which exec provides normally).  This set is exact and asserted in
# tests — it defines the surface, it does not lock anything down.
_CURATED_STDLIB = {
    "prod": math.prod,
    "pairwise": pairwise,
    "sorted": sorted,
    "math": math,
}


class ComputeError(Exception):
    """A request the endpoint rejects with a clean HTTP status + message.

    ``status`` is the HTTP status to return; ``message`` is the client-facing
    ``{"error": ...}`` body.  Used for author errors (400).
    """

    def __init__(self, status: int, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.message = message


def build_namespace(contract, window: Window, sql_fn) -> dict:
    """Assemble the exec namespace injected into a panel source.

    The metric module's panel-facing names (``metrics.__all__``), ``metric``
    from *contract*, the injected ``window``, ``pl`` (polars), ``sql``
    (the pgwire query function), and the curated stdlib names.  Returned as a
    plain dict; Python supplies ``__builtins__`` itself when this is used as
    ``exec`` globals.
    """
    ns: dict = {name: getattr(metrics, name) for name in metrics.__all__}
    ns.update(_CURATED_STDLIB)
    ns["metric"] = contract.metric
    ns["window"] = window
    ns["pl"] = pl
    ns["sql"] = sql_fn
    return ns


def run_compute(body: dict, dsn: str) -> dict:
    """Run one panel source end to end and return the neutral frame.

    *body* is the parsed request (``source`` / ``window``); *dsn* is the
    RisingWave postgres:// DSN.  Raises ``ComputeError`` for any client-visible
    failure (malformed request, contract error, author exception) so the HTTP
    layer can render a clean ``{"error": ...}`` body.
    """
    source, window = _parse_body(body)          # jwt + prefetched no longer read
    conn = rwclient.connect(dsn)
    try:
        def sql_fn(query: str, *params):
            return rwclient.query(conn, query, tuple(params))
        contract = make_contract()
        ns = build_namespace(contract, window, sql_fn)
        try:
            exec(source, ns)  # noqa: S102 — unrestricted by design (single-tenant)
        except ContractError as exc:
            raise ComputeError(400, str(exc)) from exc
        except Exception as exc:
            raise ComputeError(400, f"source error: {exc}") from exc

        reg = contract.registry
        try:
            reg.require_complete()
        except ContractError as exc:
            raise ComputeError(400, str(exc)) from exc

        log.debug("compute: output=%s", reg.output)
        try:
            result = reg.entrypoint()   # entrypoint calls sql() itself
        except Exception as exc:
            raise ComputeError(400, f"entrypoint error: {exc}") from exc

        return _to_frame(reg.output, result)
    finally:
        conn.close()


def run_plan(body: dict) -> dict:
    """Exec the source to register decorators and return ``{"bindings": {}}``.

    Performs NO data fetch and does NOT call the entrypoint.  Raises
    ``ComputeError(400, ...)`` on body/source/contract errors.

    Returns an empty bindings map — the new contract has no pre-declared
    selectors; plugins that need binding discovery should use /compute directly.
    """
    if not isinstance(body, dict):
        raise ComputeError(400, "request body must be a JSON object")
    source = body.get("source")
    if not isinstance(source, str):
        raise ComputeError(400, "missing or invalid 'source'")

    contract = make_contract()
    # sql_fn is a no-op for /plan (never calls the entrypoint or fetches data).
    ns = build_namespace(contract, Window(0, 0), lambda q, *p: pl.DataFrame())
    try:
        exec(source, ns)  # noqa: S102 — unrestricted by design (single-tenant)
    except ContractError as exc:
        raise ComputeError(400, str(exc)) from exc
    except Exception as exc:
        raise ComputeError(400, f"source error: {exc}") from exc

    reg = contract.registry
    try:
        reg.require_complete()
    except ContractError as exc:
        raise ComputeError(400, str(exc)) from exc

    return {"bindings": {}}


def _parse_body(body: object) -> tuple[str, Window]:
    """Validate the request and return ``(source, Window)``.

    Raises ``ComputeError(400, ...)`` on a non-object body, a missing/wrong-typed
    ``source`` / ``window``, or non-integer window bounds.

    The ``jwt`` and ``prefetched`` fields are no longer read — the metric pulls
    its own data via ``sql()`` at call time.
    """
    if not isinstance(body, dict):
        raise ComputeError(400, "request body must be a JSON object")
    source = body.get("source")
    win = body.get("window")
    if not isinstance(source, str):
        raise ComputeError(400, "missing or invalid 'source'")
    if not isinstance(win, dict) or "from" not in win or "to" not in win:
        raise ComputeError(400, "missing or invalid 'window' (need 'from' and 'to')")
    t0, t1 = win["from"], win["to"]
    # bool is an int subclass; reject JSON true/false slipping through as 1/0.
    if not isinstance(t0, int) or not isinstance(t1, int) or isinstance(t0, bool) or isinstance(t1, bool):
        raise ComputeError(400, "'window.from' and 'window.to' must be integer microseconds")

    return source, Window(t0, t1)


def _sanitize(v: object) -> object:
    """Replace non-finite floats with None (JSON null) so Go's encoding/json accepts them."""
    if isinstance(v, float) and not math.isfinite(v):
        return None
    return v


def _sanitize_rows(rows: list[list]) -> list[list]:
    return [[_sanitize(v) for v in row] for row in rows]


def _to_frame(output: str, result: object) -> dict:
    """Map an entrypoint return to the neutral frame for *output* mode.

    scalar -> ``{"output": "scalar", "columns": ["value"], "rows": [[v]]}`` with
    ``None`` rendered as JSON null.

    series / table accepts:
      - ``pl.DataFrame``         — columns + .rows() as-is.
      - ``list[tuple]``          — 2-tuple → columns ["ts","value"];
                                   n-tuple → columns ["c0","c1",...].
                                   Empty list → columns [] rows [].
      - ``pl.Series``            — column [series.name or "value"], one cell per row.

    Any other type raises ``ComputeError(400, ...)``.
    Non-finite floats (NaN, Inf) are replaced with None before serialization.
    """
    if output == "scalar":
        return {"output": "scalar", "columns": ["value"], "rows": [[_sanitize(result)]]}

    if isinstance(result, pl.DataFrame):
        return {
            "output": output,
            "columns": list(result.columns),
            "rows": _sanitize_rows([list(row) for row in result.rows()]),
        }

    if isinstance(result, list):
        if not result:
            return {"output": output, "columns": [], "rows": []}
        first = result[0]
        if not isinstance(first, tuple):
            raise ComputeError(
                400,
                f"entrypoint declared output={output!r}: list elements must be tuples, "
                f"got {type(first).__name__}",
            )
        width = len(first)
        columns = ["ts", "value"] if width == 2 else [f"c{i}" for i in range(width)]
        return {
            "output": output,
            "columns": columns,
            "rows": _sanitize_rows([list(t) for t in result]),
        }

    if isinstance(result, pl.Series):
        col = result.name or "value"
        return {
            "output": output,
            "columns": [col],
            "rows": _sanitize_rows([[v] for v in result]),
        }

    raise ComputeError(
        400,
        f"entrypoint declared output={output!r} must return a pl.DataFrame, "
        f"list[tuple], or pl.Series, got {type(result).__name__}",
    )
