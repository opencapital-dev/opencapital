"""``POST /compute`` — run one panel source register -> fetch -> call -> frame.

The HTTP layer (``compute.server``) delegates here.  Given a request body
``{"source", "jwt", "window": {"from", "to"}}`` this module:

  1. builds a fresh, isolated ``Contract`` (no cross-request leakage) and the
     exec namespace — the metric module's panel-facing names, ``bind`` /
     ``metric`` / ``window`` / ``pl``, plus a curated set of stdlib names the
     formulas use;
  2. ``exec``s the source so the decorators register the entrypoint, its
     selectors, and the output mode, then asserts the contract is complete;
  3. fetches each binding's org-scoped rows from read-gateway as a polars frame;
  4. calls the entrypoint with the frames by parameter name;
  5. maps the return to the NEUTRAL FRAME the P2 plugin consumes:
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

from compute import metrics
from compute.contract import ContractError, Window, make_contract
from compute.gateway import _frame_from, fetch_rows

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
    ``{"error": ...}`` body.  Used for author errors (400) and to re-surface a
    ``GatewayError``'s status faithfully.
    """

    def __init__(self, status: int, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.message = message


def build_namespace(contract, window: Window) -> dict:
    """Assemble the exec namespace injected into a panel source.

    The metric module's panel-facing names (``metrics.__all__``), ``bind`` /
    ``metric`` from *contract*, the injected ``window`` and ``pl`` (polars), and
    the curated stdlib names.  Returned as a plain dict; Python supplies
    ``__builtins__`` itself when this is used as ``exec`` globals.
    """
    ns: dict = {name: getattr(metrics, name) for name in metrics.__all__}
    ns.update(_CURATED_STDLIB)
    ns["bind"] = contract.bind
    ns["metric"] = contract.metric
    ns["window"] = window
    ns["pl"] = pl
    return ns


def run_compute(body: dict, base_url: str) -> dict:
    """Run one panel source end to end and return the neutral frame.

    *body* is the parsed request (``source`` / ``jwt`` / ``window``); *base_url*
    is the read-gateway URL.  Raises ``ComputeError`` for any client-visible
    failure (malformed request, contract error, author exception, gateway
    error) so the HTTP layer can render a clean ``{"error": ...}`` body.
    """
    source, jwt, window, prefetched = _parse_body(body)

    contract = make_contract()
    ns = build_namespace(contract, window)
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

    log.debug("compute: output=%s bindings=%d prefetched=%d", reg.output, len(reg.bindings), len(prefetched))
    fetched = {}
    for param, binding in reg.bindings.items():
        if param in prefetched:
            p = prefetched[param]
            fetched[param] = _frame_from(p["columns"], p["rows"])
        else:
            fetched[param] = fetch_rows(base_url, jwt, binding, window)

    try:
        result = reg.entrypoint(**fetched)
    except Exception as exc:
        raise ComputeError(400, f"entrypoint error: {exc}") from exc

    return _to_frame(reg.output, result)


def run_plan(body: dict) -> dict:
    """Exec the source to register decorators and return ``{"bindings": {param: raw_selector}}``.

    Performs NO data fetch and does NOT call the entrypoint.  Raises
    ``ComputeError(400, ...)`` on body/source/contract errors.
    """
    if not isinstance(body, dict):
        raise ComputeError(400, "request body must be a JSON object")
    source = body.get("source")
    if not isinstance(source, str):
        raise ComputeError(400, "missing or invalid 'source'")

    contract = make_contract()
    # Window values are irrelevant; /plan never calls the entrypoint or fetches rows.
    ns = build_namespace(contract, Window(0, 0))
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

    return {"bindings": dict(reg.raw_selectors)}


def _parse_body(body: object) -> tuple[str, str, Window, dict]:
    """Validate the request and return ``(source, jwt, Window, prefetched)``.

    Raises ``ComputeError(400, ...)`` on a non-object body, a missing/wrong-typed
    ``source`` / ``jwt`` / ``window``, or non-integer window bounds.

    ``prefetched`` is optional: missing or non-dict → ``{}``.  Each present entry
    must be ``{"columns": list, "rows": list}``; any other shape is a 400.
    """
    if not isinstance(body, dict):
        raise ComputeError(400, "request body must be a JSON object")
    source = body.get("source")
    jwt = body.get("jwt")
    win = body.get("window")
    if not isinstance(source, str):
        raise ComputeError(400, "missing or invalid 'source'")
    if not isinstance(jwt, str):
        raise ComputeError(400, "missing or invalid 'jwt'")
    if not isinstance(win, dict) or "from" not in win or "to" not in win:
        raise ComputeError(400, "missing or invalid 'window' (need 'from' and 'to')")
    t0, t1 = win["from"], win["to"]
    # bool is an int subclass; reject JSON true/false slipping through as 1/0.
    if not isinstance(t0, int) or not isinstance(t1, int) or isinstance(t0, bool) or isinstance(t1, bool):
        raise ComputeError(400, "'window.from' and 'window.to' must be integer microseconds")

    raw_pre = body.get("prefetched")
    prefetched: dict = {}
    if isinstance(raw_pre, dict):
        for param, val in raw_pre.items():
            if (
                not isinstance(val, dict)
                or not isinstance(val.get("columns"), list)
                or not isinstance(val.get("rows"), list)
            ):
                raise ComputeError(400, f"prefetched[{param!r}] must have 'columns' (list) and 'rows' (list)")
            prefetched[param] = val

    return source, jwt, Window(t0, t1), prefetched


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
