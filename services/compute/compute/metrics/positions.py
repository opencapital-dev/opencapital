"""Per-instrument position table reconstruction.

Replaces the retired ``positions`` rw_template: the read-gateway now serves only
raw scoped views, so the per-instrument summary that fed the position_stats
panels is reconstructed here from four normalized views.

Inputs (real wire shape; ``ts`` int-µs):
  closures   — e_closures   : instrument, realized_pnl, holding_seconds (window)
  cycles     — e_cycles     : instrument, pnl_base, duration_sec, was_re_entry (window)
  events     — e_events     : instrument, event_type (window)
  instrument — e_instrument : instrument, ts, quantity, currency, current_value,
                              unrealized_pnl (as-of; latest tick per instrument)

Reproduces the old SQL exactly: a full-outer-join across four sub-aggregates
keyed by instrument, COALESCE defaults, latest-tick as-of, and the derived
total_pnl / is_open / roi / position_irr columns.
"""

from __future__ import annotations

import polars as pl

_COLUMNS = [
    "instrument_id", "currency", "n_trades", "n_round_trips",
    "realized_pnl", "unrealized_pnl", "total_pnl", "current_qty",
    "current_value", "is_open", "holding_seconds", "roi", "position_irr",
]


def _empty() -> pl.DataFrame:
    return pl.DataFrame(
        {c: [] for c in _COLUMNS},
        schema={
            "instrument_id": pl.Utf8, "currency": pl.Utf8,
            "n_trades": pl.Float64, "n_round_trips": pl.Float64,
            "realized_pnl": pl.Float64, "unrealized_pnl": pl.Float64,
            "total_pnl": pl.Float64, "current_qty": pl.Float64,
            "current_value": pl.Float64, "is_open": pl.Float64,
            "holding_seconds": pl.Float64, "roi": pl.Float64,
            "position_irr": pl.Float64,
        },
    )


def _realized(closures: pl.DataFrame) -> pl.DataFrame:
    if closures.height == 0:
        return pl.DataFrame(schema={
            "instrument": pl.Utf8, "realized_pnl": pl.Float64,
            "holding_seconds": pl.Float64,
        })
    return closures.group_by("instrument").agg(
        pl.col("realized_pnl").sum().alias("realized_pnl"),
        pl.col("holding_seconds").sum().alias("holding_seconds"),
    )


def _round_trips(cycles: pl.DataFrame) -> pl.DataFrame:
    if cycles.height == 0:
        return pl.DataFrame(schema={"instrument": pl.Utf8, "n_round_trips": pl.Float64})
    return cycles.group_by("instrument").agg(
        pl.len().cast(pl.Float64).alias("n_round_trips")
    )


def _trades(events: pl.DataFrame) -> pl.DataFrame:
    if events.height == 0:
        return pl.DataFrame(schema={"instrument": pl.Utf8, "n_trades": pl.Float64})
    return (
        events.filter(pl.col("event_type") == "TRADE")
        .group_by("instrument")
        .agg(pl.len().cast(pl.Float64).alias("n_trades"))
    )


def _latest_eq(instrument: pl.DataFrame) -> pl.DataFrame:
    cols = {"instrument": pl.Utf8, "quantity": pl.Float64, "currency": pl.Utf8,
            "current_value": pl.Float64, "unrealized_pnl": pl.Float64}
    if instrument.height == 0:
        return pl.DataFrame(schema=cols)
    return (
        instrument.sort("ts")
        .group_by("instrument", maintain_order=False)
        .agg(
            pl.col("quantity").last(),
            pl.col("currency").last(),
            pl.col("current_value").last(),
            pl.col("unrealized_pnl").last(),
        )
    )


def positions(
    closures: pl.DataFrame,
    cycles: pl.DataFrame,
    events: pl.DataFrame,
    instrument: pl.DataFrame,
) -> pl.DataFrame:
    """Reconstruct the per-instrument position table from raw scoped views.

    One row per instrument that appears in any of the four inputs. Columns match
    the old ``positions`` SELECT (``instrument_id`` keyed); see module docstring.
    """
    realized = _realized(closures)
    round_trips = _round_trips(cycles)
    trades = _trades(events)
    latest = _latest_eq(instrument)

    inst = (
        realized
        .join(latest, on="instrument", how="full", coalesce=True)
        .join(round_trips, on="instrument", how="full", coalesce=True)
        .join(trades, on="instrument", how="full", coalesce=True)
    )
    if inst.height == 0:
        return _empty()

    out = inst.select(
        pl.col("instrument").alias("instrument_id"),
        pl.col("currency").fill_null(""),
        pl.col("n_trades").fill_null(0.0),
        pl.col("n_round_trips").fill_null(0.0),
        pl.col("realized_pnl").fill_null(0.0),
        pl.col("unrealized_pnl").fill_null(0.0),
        pl.col("quantity").fill_null(0.0).alias("current_qty"),
        pl.col("current_value").fill_null(0.0),
        pl.col("holding_seconds").fill_null(0.0),
    ).with_columns(
        (pl.col("realized_pnl") + pl.col("unrealized_pnl")).alias("total_pnl"),
    ).with_columns(
        pl.when(pl.col("current_qty") > 0).then(1.0).otherwise(0.0).alias("is_open"),
        pl.when(pl.col("current_value").abs() > 0)
        .then(pl.col("total_pnl") / pl.col("current_value"))
        .otherwise(None)
        .alias("roi"),
        pl.lit(None, dtype=pl.Float64).alias("position_irr"),
    )

    return out.select(_COLUMNS)
