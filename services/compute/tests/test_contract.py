"""Registration-machinery tests for the panel-source contract.

Exercises ``make_contract()`` the way the endpoint will: build an exec
namespace from the contract, ``exec`` a fixture source, then read the resulting
registry.  Registration must NOT perform any fetch — the metric function pulls
its own data via ``sql()`` at call time.
"""

from __future__ import annotations

import pytest

from compute.contract import Contract, ContractError, Window, make_contract


def _exec_source(src: str, contract: Contract, extra: dict | None = None) -> None:
    """Run *src* in a namespace wired from *contract* (+ optional extras)."""
    ns: dict = {"metric": contract.metric}
    if extra:
        ns.update(extra)
    exec(src, ns)


SOURCE_METRIC = r'''
@metric(output="series")
def equity_curve():
    return []
'''


def test_registers_entrypoint_and_output() -> None:
    contract = make_contract()
    _exec_source(SOURCE_METRIC, contract)
    reg = contract.registry

    assert reg.output == "series"
    assert reg.entrypoint is not None
    assert reg.entrypoint.__name__ == "equity_curve"


def test_entrypoint_is_callable() -> None:
    contract = make_contract()
    _exec_source(
        r'''
@metric(output="scalar")
def m():
    return 42
''',
        contract,
    )
    assert contract.registry.entrypoint() == 42


def test_no_metric_raises_on_completeness_check() -> None:
    contract = make_contract()
    _exec_source("x = 1  # no @metric", contract)
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


def test_per_exec_isolation() -> None:
    c1 = make_contract()
    c2 = make_contract()
    _exec_source(
        r'''
@metric(output="scalar")
def first():
    return 1
''',
        c1,
    )
    _exec_source(
        r'''
@metric(output="table")
def second():
    return []
''',
        c2,
    )
    assert c1.registry.entrypoint.__name__ == "first"
    assert c2.registry.entrypoint.__name__ == "second"
    assert c1.registry.output == "scalar"
    assert c2.registry.output == "table"


def test_window_unpacks_to_pair() -> None:
    w = Window(1_000, 2_000)
    t0, t1 = w
    assert (t0, t1) == (1_000, 2_000)
    assert w.t0 == 1_000
    assert w.t1 == 2_000
