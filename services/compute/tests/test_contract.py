"""Registration-machinery tests for the panel-source contract.

Exercises ``make_contract()`` the way the T7 endpoint will: build an exec
namespace from the contract + the metric module, ``exec`` a fixture source,
then read the resulting registry.  Registration must NOT perform any fetch.
"""

from __future__ import annotations

import pytest

from compute.contract import Binding, Contract, ContractError, Window, make_contract


def _exec_source(src: str, contract: Contract, extra: dict | None = None) -> None:
    """Run *src* in a namespace wired from *contract* (+ optional extras)."""
    ns: dict = {"bind": contract.bind, "metric": contract.metric}
    if extra:
        ns.update(extra)
    exec(src, ns)


SOURCE_BIND_OUTER = r'''
@bind(
    nav   = "nav{portfolio=\"$pid\"} @asof",
    flows = "flows{portfolio=\"$pid\"} @window",
)
@metric(output="series")
def equity_curve(nav, flows):
    return nav
'''

SOURCE_METRIC_OUTER = r'''
@metric(output="series")
@bind(
    nav   = "nav{portfolio=\"$pid\"} @asof",
    flows = "flows{portfolio=\"$pid\"} @window",
)
def equity_curve(nav, flows):
    return nav
'''


@pytest.mark.parametrize("src", [SOURCE_BIND_OUTER, SOURCE_METRIC_OUTER])
def test_registers_entrypoint_bindings_and_output(src: str) -> None:
    contract = make_contract()
    _exec_source(src, contract)
    reg = contract.registry

    assert reg.output == "series"
    assert reg.entrypoint is not None
    assert reg.entrypoint.__name__ == "equity_curve"

    assert list(reg.bindings) == ["nav", "flows"]
    assert reg.bindings["nav"] == Binding("nav{portfolio=\"$pid\"}", "asof")
    assert reg.bindings["flows"] == Binding("flows{portfolio=\"$pid\"}", "window")


def test_mode_absent_is_none() -> None:
    contract = make_contract()
    _exec_source(
        r'''
@metric(output="scalar")
@bind(x="series_a{portfolio=\"$pid\"}")
def m(x):
    return 1.0
''',
        contract,
    )
    assert contract.registry.bindings["x"] == Binding("series_a{portfolio=\"$pid\"}", None)


def test_at_sign_inside_value_is_not_a_mode() -> None:
    # An @ inside a quoted matcher value is part of the selector, not the mode
    # (matches the Go DSL parser, which only reads @ after the final }).
    contract = make_contract()
    _exec_source(
        '''
@metric(output="scalar")
@bind(
    a='events{instrument="a@b"}',
    c='events{instrument="x@y"} @window',
)
def m(a, c):
    return 1.0
''',
        contract,
    )
    assert contract.registry.bindings["a"] == Binding('events{instrument="a@b"}', None)
    assert contract.registry.bindings["c"] == Binding('events{instrument="x@y"}', "window")


def test_entrypoint_is_callable_with_fetched_frames() -> None:
    contract = make_contract()
    _exec_source(SOURCE_BIND_OUTER, contract)
    # The endpoint will call entrypoint(**fetched). Prove the function survives unchanged.
    assert contract.registry.entrypoint(nav="N", flows="F") == "N"


def test_no_metric_raises_on_completeness_check() -> None:
    contract = make_contract()
    # A source with only @bind (no @metric) execs fine — helpers are allowed —
    # but is an incomplete contract: completeness check surfaces it.
    _exec_source(
        r'''
@bind(x="a{portfolio=\"$pid\"}")
def m(x):
    return x
''',
        contract,
    )
    with pytest.raises(ContractError, match="no @metric"):
        contract.registry.require_complete()


def test_two_metrics_raises() -> None:
    contract = make_contract()
    with pytest.raises(ContractError, match="more than one @metric"):
        _exec_source(
            '''
@metric(output="scalar")
def a():
    return 1.0

@metric(output="scalar")
def b():
    return 2.0
''',
            contract,
        )


def test_bad_output_raises() -> None:
    contract = make_contract()
    with pytest.raises(ContractError, match="invalid output"):
        _exec_source(
            '''
@metric(output="vector")
def a():
    return 1.0
''',
            contract,
        )


def test_bad_mode_raises() -> None:
    contract = make_contract()
    with pytest.raises(ContractError, match="invalid @mode"):
        _exec_source(
            r'''
@metric(output="series")
@bind(x="a{portfolio=\"$pid\"} @rolling")
def m(x):
    return x
''',
            contract,
        )


def test_bind_param_mismatch_raises() -> None:
    contract = make_contract()
    with pytest.raises(ContractError, match="do not match"):
        _exec_source(
            r'''
@metric(output="series")
@bind(typo="a{portfolio=\"$pid\"} @asof")
def m(x):
    return x
''',
            contract,
        )


def test_per_exec_isolation() -> None:
    c1 = make_contract()
    c2 = make_contract()
    _exec_source(
        r'''
@metric(output="scalar")
@bind(a="one{portfolio=\"$pid\"} @latest")
def first(a):
    return a
''',
        c1,
    )
    _exec_source(
        r'''
@metric(output="table")
@bind(b="two{portfolio=\"$pid\"} @window")
def second(b):
    return b
''',
        c2,
    )
    assert c1.registry.entrypoint.__name__ == "first"
    assert c2.registry.entrypoint.__name__ == "second"
    assert list(c1.registry.bindings) == ["a"]
    assert list(c2.registry.bindings) == ["b"]
    assert c1.registry.output == "scalar"
    assert c2.registry.output == "table"


def test_window_unpacks_to_pair() -> None:
    w = Window(1_000, 2_000)
    t0, t1 = w
    assert (t0, t1) == (1_000, 2_000)
    assert w.t0 == 1_000
    assert w.t1 == 2_000
