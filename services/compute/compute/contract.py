"""Panel-source contract runtime — ``@metric`` + a per-exec registry.

A panel's *source* is self-contained Python that declares its output via the
``@metric`` decorator and pulls its own data by calling ``sql(...)`` inside
the function body.  Example::

    @metric(output="series")
    def equity_curve():
        df = sql("SELECT ts, nav FROM nav WHERE portfolio = $1", portfolio_id)
        t0, t1 = window
        ...

This module only builds the *registration* machinery.  The compute endpoint
runs register -> call: it ``exec``s the source so the decorator REGISTERS the
entrypoint and the output mode (no pre-declared bindings, no pre-fetch),
then calls the entrypoint.  The entrypoint calls ``sql()`` — injected into the
exec namespace by the endpoint — to pull its own data.

Per-exec isolation
------------------
``make_contract()`` returns a fresh ``Contract`` whose ``.metric`` closes over
a private ``Registry``.  There is no module-global mutable state, so two
concurrent execs cannot bleed into one another.  The endpoint wires it as::

    contract = make_contract()
    ns = {**metric_module_names, "metric": contract.metric,
          "window": window, "pl": polars, "sql": sql_fn}
    exec(source, ns)
    contract.registry.require_complete()
    result = contract.registry.entrypoint()
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable, Literal, NamedTuple

OutputMode = Literal["scalar", "series", "table"]

_OUTPUTS: frozenset[str] = frozenset({"scalar", "series", "table"})


class ContractError(Exception):
    """A malformed panel-source contract (bad decorators)."""


class Window(NamedTuple):
    """Injected dashboard time range as an int-microsecond pair.

    Unpacks directly as ``t0, t1 = window`` and also exposes ``.t0`` / ``.t1``.
    """

    t0: int
    t1: int


@dataclass(slots=True)
class Registry:
    """Per-exec capture of one panel source's contract.

    Holds the single ``@metric`` entrypoint and the output mode.  Populated by
    the decorator obtained from the owning ``Contract``; never shared across
    execs.
    """

    entrypoint: Callable | None = None
    output: OutputMode | None = None

    def require_complete(self) -> None:
        """Assert the registry holds a usable contract; raise ``ContractError`` otherwise.

        Checks that exactly one ``@metric`` entrypoint was registered.
        """
        if self.entrypoint is None:
            raise ContractError("no @metric entrypoint declared in source")


@dataclass(slots=True)
class Contract:
    """A per-exec contract: a fresh ``Registry`` plus the ``metric`` decorator bound to it.

    Inject ``contract.metric`` into the exec namespace as ``metric``; read the
    populated contract back from ``contract.registry`` after the source has been
    ``exec``d.
    """

    registry: Registry = field(default_factory=Registry)

    def metric(self, *, output: str) -> Callable[[Callable], Callable]:
        """Mark the decorated function as THE entrypoint and record *output*.

        ``output`` must be one of ``scalar`` / ``series`` / ``table``.  Raises
        ``ContractError`` on an invalid output or a second ``@metric``.  Returns
        the function unchanged.
        """
        if output not in _OUTPUTS:
            raise ContractError(
                f"invalid output {output!r}; expected one of {sorted(_OUTPUTS)}"
            )
        reg = self.registry
        if reg.entrypoint is not None:
            raise ContractError("more than one @metric entrypoint declared in source")

        def decorate(fn: Callable) -> Callable:
            reg.entrypoint = fn
            reg.output = output  # type: ignore[assignment]
            return fn

        return decorate


def make_contract() -> Contract:
    """Create a fresh, isolated ``Contract`` for one source exec.

    Returns a ``Contract`` whose ``.metric`` writes into its own private
    ``.registry``.  No state is shared with any other contract.
    """
    return Contract()
