"""Unit tests for compute.metrics.positions.

Parity oracle: the OLD `positions` rw_template SQL (full-outer-join of four
window/as-of sub-aggregates keyed by instrument, COALESCE defaults, is_open /
roi derivations). Each scenario hand-builds the four normalized view frames in
their real wire shape (int-µs ts + friendly columns) and asserts the
reconstructed per-instrument table cell by cell.

Edge cases covered:
  - full-outer-join across realized / round-trips / trades / latest-tick keyed
    by instrument, including instruments present in only one sub-aggregate;
  - COALESCE defaults (missing realized -> 0, missing currency -> "");
  - latest-tick as-of: the latest instrument row per instrument by ts wins;
  - is_open = current_qty > 0; roi = total_pnl / current_value (None when
    current_value ~ 0); position_irr always None.
"""

from __future__ import annotations

import math

import polars as pl

from compute.metrics import positions

_D = 86_400_000_000  # one day in microseconds


def _closures(rows: list[tuple]) -> pl.DataFrame:
    return pl.DataFrame(
        {
            "org_id": [r[0] for r in rows],
            "portfolio": [r[1] for r in rows],
            "instrument": [r[2] for r in rows],
            "ts": [r[3] for r in rows],
            "realized_pnl": [r[4] for r in rows],
            "holding_seconds": [r[5] for r in rows],
        },
        schema_overrides={"ts": pl.Int64},
    )


def _cycles(rows: list[tuple]) -> pl.DataFrame:
    return pl.DataFrame(
        {
            "org_id": [r[0] for r in rows],
            "portfolio": [r[1] for r in rows],
            "instrument": [r[2] for r in rows],
            "ts": [r[3] for r in rows],
            "pnl_base": [r[4] for r in rows],
            "duration_sec": [r[5] for r in rows],
            "was_re_entry": [r[6] for r in rows],
        },
        schema_overrides={"ts": pl.Int64},
    )


def _events(rows: list[tuple]) -> pl.DataFrame:
    return pl.DataFrame(
        {
            "org_id": [r[0] for r in rows],
            "portfolio": [r[1] for r in rows],
            "instrument": [r[2] for r in rows],
            "ts": [r[3] for r in rows],
            "event_type": [r[4] for r in rows],
        },
        schema_overrides={"ts": pl.Int64},
    )


def _instrument(rows: list[tuple]) -> pl.DataFrame:
    return pl.DataFrame(
        {
            "org_id": [r[0] for r in rows],
            "portfolio": [r[1] for r in rows],
            "instrument": [r[2] for r in rows],
            "ts": [r[3] for r in rows],
            "quantity": [r[4] for r in rows],
            "currency": [r[5] for r in rows],
            "current_value": [r[6] for r in rows],
            "unrealized_pnl": [r[7] for r in rows],
        },
        schema_overrides={"ts": pl.Int64},
    )


def _by_instrument(df: pl.DataFrame) -> dict[str, dict]:
    return {row["instrument_id"]: row for row in df.to_dicts()}


def test_positions_multi_instrument_full_outer_join() -> None:
    # AAA: closures + cycles + trades + a latest tick (two ticks, latest wins).
    # BBB: trades + a latest tick, no closures/cycles.
    # CCC: closures only (closed out; latest tick shows it flat -> not open).
    # DDD: latest tick only (held, never traded in window) -> open.
    closures = _closures(
        [
            ("o", "p", "AAA", 5 * _D, 100.0, 3600.0),
            ("o", "p", "AAA", 6 * _D, 50.0, 7200.0),
            ("o", "p", "CCC", 4 * _D, -30.0, 1800.0),
        ]
    )
    cycles = _cycles(
        [
            ("o", "p", "AAA", 5 * _D, 100.0, 3600.0, 0.0),
            ("o", "p", "AAA", 6 * _D, 50.0, 7200.0, 1.0),
        ]
    )
    events = _events(
        [
            ("o", "p", "AAA", 5 * _D, "TRADE"),
            ("o", "p", "AAA", 6 * _D, "TRADE"),
            ("o", "p", "AAA", 6 * _D, "DIVIDEND"),
            ("o", "p", "BBB", 2 * _D, "TRADE"),
        ]
    )
    instrument = _instrument(
        [
            ("o", "p", "AAA", 5 * _D, 10.0, "USD", 1000.0, 25.0),
            ("o", "p", "AAA", 7 * _D, 12.0, "USD", 1200.0, 40.0),
            ("o", "p", "BBB", 2 * _D, 5.0, "EUR", 500.0, -10.0),
            ("o", "p", "CCC", 4 * _D, 0.0, "GBP", 0.0, 0.0),
            ("o", "p", "DDD", 1 * _D, 3.0, "JPY", 300.0, 15.0),
        ]
    )

    out = positions(closures, cycles, events, instrument)
    rows = _by_instrument(out)

    assert set(rows) == {"AAA", "BBB", "CCC", "DDD"}

    aaa = rows["AAA"]
    assert aaa["currency"] == "USD"
    assert aaa["n_trades"] == 2.0          # only event_type == TRADE counted
    assert aaa["n_round_trips"] == 2.0
    assert aaa["realized_pnl"] == 150.0
    assert aaa["unrealized_pnl"] == 40.0   # latest tick (7d) wins over 5d tick
    assert aaa["total_pnl"] == 190.0
    assert aaa["current_qty"] == 12.0
    assert aaa["current_value"] == 1200.0
    assert aaa["is_open"] == 1.0
    assert aaa["holding_seconds"] == 10800.0
    assert aaa["roi"] == 190.0 / 1200.0
    assert aaa["position_irr"] is None

    bbb = rows["BBB"]
    assert bbb["currency"] == "EUR"
    assert bbb["n_trades"] == 1.0
    assert bbb["n_round_trips"] == 0.0     # COALESCE default
    assert bbb["realized_pnl"] == 0.0      # no closures -> 0
    assert bbb["unrealized_pnl"] == -10.0
    assert bbb["total_pnl"] == -10.0
    assert bbb["current_qty"] == 5.0
    assert bbb["current_value"] == 500.0
    assert bbb["is_open"] == 1.0
    assert bbb["holding_seconds"] == 0.0
    assert bbb["roi"] == -10.0 / 500.0

    ccc = rows["CCC"]
    assert ccc["currency"] == "GBP"
    assert ccc["n_trades"] == 0.0
    assert ccc["n_round_trips"] == 0.0
    assert ccc["realized_pnl"] == -30.0
    assert ccc["unrealized_pnl"] == 0.0
    assert ccc["total_pnl"] == -30.0
    assert ccc["current_qty"] == 0.0
    assert ccc["current_value"] == 0.0
    assert ccc["is_open"] == 0.0           # flat -> not open
    assert ccc["holding_seconds"] == 1800.0
    assert ccc["roi"] is None              # current_value ~ 0 -> None

    ddd = rows["DDD"]
    assert ddd["currency"] == "JPY"
    assert ddd["n_trades"] == 0.0
    assert ddd["n_round_trips"] == 0.0
    assert ddd["realized_pnl"] == 0.0
    assert ddd["unrealized_pnl"] == 15.0
    assert ddd["total_pnl"] == 15.0
    assert ddd["current_qty"] == 3.0
    assert ddd["current_value"] == 300.0
    assert ddd["is_open"] == 1.0
    assert ddd["holding_seconds"] == 0.0
    assert ddd["roi"] == 15.0 / 300.0


def test_positions_instrument_absent_from_ticks_coalesces_currency() -> None:
    # An instrument that closed/traded in the window but has NO latest tick at
    # all: currency COALESCEs to "", quantity/value/unrealized default to 0,
    # is_open=0, roi=None.
    closures = _closures([("o", "p", "ZZZ", 3 * _D, 75.0, 600.0)])
    cycles = _cycles([("o", "p", "ZZZ", 3 * _D, 75.0, 600.0, 0.0)])
    events = _events([("o", "p", "ZZZ", 3 * _D, "TRADE")])
    instrument = _instrument([])

    out = positions(closures, cycles, events, instrument)
    rows = _by_instrument(out)

    assert set(rows) == {"ZZZ"}
    zzz = rows["ZZZ"]
    assert zzz["currency"] == ""
    assert zzz["n_trades"] == 1.0
    assert zzz["n_round_trips"] == 1.0
    assert zzz["realized_pnl"] == 75.0
    assert zzz["unrealized_pnl"] == 0.0
    assert zzz["total_pnl"] == 75.0
    assert zzz["current_qty"] == 0.0
    assert zzz["current_value"] == 0.0
    assert zzz["is_open"] == 0.0
    assert zzz["roi"] is None
    assert zzz["position_irr"] is None


def test_positions_all_empty_returns_empty_table() -> None:
    out = positions(_closures([]), _cycles([]), _events([]), _instrument([]))
    assert out.height == 0
    assert out.columns == [
        "instrument_id", "currency", "n_trades", "n_round_trips",
        "realized_pnl", "unrealized_pnl", "total_pnl", "current_qty",
        "current_value", "is_open", "holding_seconds", "roi", "position_irr",
    ]


def test_positions_only_non_trade_events_not_counted() -> None:
    # events frame carries DIVIDEND/CASHFLOW too; n_trades counts TRADE only.
    closures = _closures([])
    cycles = _cycles([])
    events = _events(
        [
            ("o", "p", "AAA", 2 * _D, "DIVIDEND"),
            ("o", "p", "AAA", 3 * _D, "CASHFLOW"),
        ]
    )
    instrument = _instrument([("o", "p", "AAA", 2 * _D, 4.0, "USD", 400.0, 8.0)])

    out = positions(closures, cycles, events, instrument)
    rows = _by_instrument(out)
    assert rows["AAA"]["n_trades"] == 0.0
    assert rows["AAA"]["is_open"] == 1.0


def test_positions_negative_value_roi_defined() -> None:
    # A short with negative current_value still gets a roi (ABS(value) > 0).
    closures = _closures([])
    cycles = _cycles([])
    events = _events([])
    instrument = _instrument([("o", "p", "SH", 1 * _D, -5.0, "USD", -250.0, -12.0)])

    out = positions(closures, cycles, events, instrument)
    sh = _by_instrument(out)["SH"]
    assert sh["current_value"] == -250.0
    assert sh["current_qty"] == -5.0
    assert sh["is_open"] == 0.0           # current_qty > 0 is False for a short
    assert sh["total_pnl"] == -12.0
    assert sh["roi"] == -12.0 / -250.0
    assert not math.isnan(sh["roi"])
