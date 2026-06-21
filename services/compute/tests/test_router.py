import pytest
from compute.router import tables_in, decide_store

def test_tables_in_simple_and_join():
    assert tables_in("SELECT ts, nav FROM portfolio_per_tick WHERE portfolio_id=$1") == {"portfolio_per_tick"}
    assert tables_in("SELECT * FROM a JOIN b ON a.id=b.id") == {"a", "b"}
    assert tables_in("SELECT * FROM yfinance.gw_classification") == {"gw_classification"}

def test_decide_store_unambiguous():
    cat = {"portfolio_per_tick": "rw", "gw_classification": "pg"}
    assert decide_store({"portfolio_per_tick"}, cat) == "rw"
    assert decide_store({"gw_classification"}, cat) == "pg"

def test_decide_store_portfolios_overlap_defaults_rw():
    assert decide_store({"portfolios"}, {"portfolios": "both"}) == "rw"

def test_decide_store_unknown_table_errors():
    with pytest.raises(ValueError, match="not found"):
        decide_store({"nope"}, {"portfolio_per_tick": "rw"})

def test_decide_store_cross_store_errors():
    cat = {"a": "rw", "b": "pg"}
    with pytest.raises(ValueError, match="across"):
        decide_store({"a", "b"}, cat)
