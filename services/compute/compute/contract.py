"""Panel-source contract runtime — ``@bind`` / ``@metric`` + a per-exec registry.

A panel's *source* is self-contained Python that declares its data and output
via two decorators; the function body stays plain polars.  Example::

    @bind(
        nav   = "nav{portfolio=$portfolio_id} @asof",
        flows = "flows{portfolio=$portfolio_id} @window",
    )
    @metric(output="series")
    def equity_curve(nav, flows):      # nav, flows: polars DataFrames
        t0, t1 = window                 # injected dashboard time range
        ...

This module only builds the *registration* machinery.  The compute endpoint
(T7) runs register -> fetch -> call: it ``exec``s the source so the decorators
REGISTER the entrypoint, its bindings, and the output mode (no fetch happens
here), then fetches each binding's rows and calls the entrypoint.

Per-exec isolation
------------------
``make_contract()`` returns a fresh ``Contract`` whose ``.bind`` / ``.metric``
close over a private ``Registry``.  There is no module-global mutable state, so
two concurrent execs cannot bleed into one another.  T7 wires it as::

    contract = make_contract()
    ns = {**metric_module_names, "bind": contract.bind, "metric": contract.metric,
          "window": window, "pl": polars}
    exec(source, ns)
    contract.registry.require_complete()
    # ... fetch each binding, then contract.registry.entrypoint(**fetched)
"""

from __future__ import annotations

import inspect
from dataclasses import dataclass, field
from typing import Callable, Literal, NamedTuple

OutputMode = Literal["scalar", "series", "table"]
BindMode = Literal["asof", "window", "latest"]

_OUTPUTS: frozenset[str] = frozenset({"scalar", "series", "table"})
_MODES: frozenset[str] = frozenset({"asof", "window", "latest"})


class ContractError(Exception):
    """A malformed panel-source contract (bad decorators / mismatched params)."""


class Window(NamedTuple):
    """Injected dashboard time range as an int-microsecond pair.

    Unpacks directly as ``t0, t1 = window`` and also exposes ``.t0`` / ``.t1``.
    """

    t0: int
    t1: int


@dataclass(frozen=True, slots=True)
class Binding:
    """One declared data input: an org-scoped selector plus an optional mode.

    ``selector`` is the selector string with any trailing ``@mode`` stripped off.
    ``mode`` is one of ``asof`` / ``window`` / ``latest``, or ``None`` when the
    selector carried no trailing ``@mode``.
    """

    selector: str
    mode: BindMode | None


def _parse_selector(raw: str) -> Binding:
    """Split a trailing ``@mode`` off *raw* and validate it.

    ``"nav{portfolio=$pid} @asof"`` → ``Binding("nav{portfolio=$pid}", "asof")``.
    A selector with no ``@mode`` yields ``mode=None``.  Raises ``ContractError``
    on an unrecognised mode token.

    Only an ``@`` *after the final* ``}`` is the mode, matching the Go DSL
    parser; an ``@`` inside a quoted matcher value (e.g. ``{x="a@b"}``) is part
    of the selector, not a mode.
    """
    raw = raw.strip()
    at = raw.find("@", raw.rfind("}") + 1)
    if at == -1:
        return Binding(raw, None)
    mode = raw[at + 1:].strip()
    if mode not in _MODES:
        raise ContractError(
            f"invalid @mode {mode!r}; expected one of {sorted(_MODES)}"
        )
    return Binding(raw[:at].strip(), mode)  # type: ignore[arg-type]


@dataclass(slots=True)
class Registry:
    """Per-exec capture of one panel source's contract.

    Holds the single ``@metric`` entrypoint, the ordered ``param -> Binding``
    map declared by ``@bind``, and the output mode.  Populated by the decorators
    obtained from the owning ``Contract``; never shared across execs.

    ``raw_selectors`` maps each param to the selector string exactly as the
    author wrote it (prefix + matchers + @mode), for ``/plan`` to return.
    """

    entrypoint: Callable | None = None
    bindings: dict[str, Binding] = field(default_factory=dict)
    raw_selectors: dict[str, str] = field(default_factory=dict)
    output: OutputMode | None = None

    def require_complete(self) -> None:
        """Assert the registry holds a usable contract; raise ``ContractError`` otherwise.

        Checks that exactly one ``@metric`` entrypoint was registered.  Binding
        param-name agreement is enforced eagerly by the decorators, so a complete
        registry is ready for fetch + call.
        """
        if self.entrypoint is None:
            raise ContractError("no @metric entrypoint declared in source")


def _entry_params(fn: Callable) -> set[str]:
    return set(inspect.signature(fn).parameters)


@dataclass(slots=True)
class Contract:
    """A per-exec contract: a fresh ``Registry`` plus the decorators bound to it.

    Inject ``contract.bind`` and ``contract.metric`` into the exec namespace as
    ``bind`` / ``metric``; read the populated contract back from
    ``contract.registry`` after the source has been ``exec``d.
    """

    registry: Registry = field(default_factory=Registry)

    def bind(self, **selectors: str) -> Callable[[Callable], Callable]:
        """Declare this function's data inputs; record them in the registry.

        Each keyword is a function PARAMETER NAME and each value a selector
        string with an optional trailing ``@mode``.  Parses + strips the mode,
        records ``{param: Binding(selector, mode)}``, and returns the function
        unchanged.  Performs NO fetch.
        """

        def decorate(fn: Callable) -> Callable:
            for param, raw in selectors.items():
                self.registry.bindings[param] = _parse_selector(raw)
                self.registry.raw_selectors[param] = raw
            self._check_param_match(fn)
            return fn

        return decorate

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
            self._check_param_match(fn)
            return fn

        return decorate

    def _check_param_match(self, fn: Callable) -> None:
        """Fail fast if a ``@bind`` param has no matching entrypoint parameter.

        Only the entrypoint is checked (helpers may be ``@bind``-free but are
        never themselves bound).  Runs from whichever decorator completes the
        pair, so it covers both decorator orders.
        """
        reg = self.registry
        if reg.entrypoint is not fn or not reg.bindings:
            return
        params = _entry_params(fn)
        unknown = [p for p in reg.bindings if p not in params]
        if unknown:
            raise ContractError(
                f"@bind param(s) {unknown} do not match the parameters of "
                f"entrypoint {fn.__name__!r} ({sorted(params)})"
            )


def make_contract() -> Contract:
    """Create a fresh, isolated ``Contract`` for one source exec.

    Returns a ``Contract`` whose ``.bind`` / ``.metric`` write into its own
    private ``.registry``.  No state is shared with any other contract.
    """
    return Contract()
