# `portfolio_events.v1` payload shapes

Companion to `schemas/portfolio_events.v1.avsc`. The Avro envelope is core-owned
and strict-typed (see the `.avsc`). The `payload` field is a JSON string whose
shape depends on `event_type`. This file documents each shape.

Conventions:
- All currency-bearing amounts are in **native currency** unless suffixed `_base`.
- `fx_rate_to_base` is "base currency units per 1 unit of native currency" — set
  by the producer when the broker reports it; nullable otherwise (option 2 —
  ADR-0012).
- Field types are JSON: numbers default to double; integers explicit when used.
- Optional fields use `null` (not absent) for missing values when the field is
  defined.

---

## `event_type = "TRADE"`

A portfolio buy/sell execution. `source_id` = `trade_id`; `instrument_id` = the
traded instrument.

```json
{
  "side":             "buy",        // string: "buy" | "sell"
  "price":            150.25,       // double: execution price in `currency`
  "quantity":         10.0,         // double: signed by `side` at fold time
  "currency":         "USD",        // string: ISO 4217
  "venue":            "NYSE",       // string
  "commission":       1.0,          // nullable double
  "fees":             0.05,         // nullable double

  "fx_rate_to_base":  0.92,         // nullable double — set when broker auto-converted to base
  "fx_fees_native":   2.50,         // nullable double — only when fx_rate_to_base set
  "fx_fees_currency": "USD"         // nullable string — only when fx_rate_to_base set
}
```

Settlement modes (mutually exclusive per trade):
- **Trade-currency settlement**: `fx_rate_to_base = null`. Settles in
  `CASH:<currency>`. Cross-currency move (if any) is modelled by a separate
  `FX_CONVERSION` event.
- **Base-currency settlement**: `fx_rate_to_base` set. Settles directly in
  `CASH:<base>` at exactly this rate.

---

## `event_type = "DIVIDEND"`

A dividend payment. `source_id` = `dividend_id`; `instrument_id` = the issuing
instrument.

```json
{
  "gross_native":       12.50,
  "withholding_native": 1.875,      // nullable double
  "currency":           "USD",

  "fx_rate_to_base":  0.92,         // nullable double
  "fx_fees_native":   null,         // nullable double
  "fx_fees_currency": null          // nullable string
}
```

Same settlement modes as TRADE.

---

## `event_type = "CASHFLOW"`

A non-trade cash movement: deposit, withdrawal, interest accrual, or fee.
`source_id` = `cashflow_id`; `instrument_id` = `null` (cashflows don't reference
an instrument).

```json
{
  "type":          "DEPOSIT",       // string: "DEPOSIT" | "WITHDRAWAL" | "INTEREST_ON_CASH" | "FEE"
  "amount_native": 1000.0,          // double: signed positive; the `type` determines direction at fold time
  "currency":      "USD"
}
```

No FX field — cross-currency cashflows are modelled by a separate FX_CONVERSION
event after the deposit lands.

---

## `event_type = "FX_CONVERSION"`

A broker-executed currency conversion. `source_id` = `fx_conversion_id`;
`instrument_id` = `null`.

```json
{
  "from_currency": "USD",
  "from_amount":   1000.0,          // double
  "to_currency":   "EUR",
  "to_amount":     920.0,           // double
  "rate":          0.92,            // double: to_amount / from_amount
  "fees_native":   2.50,            // nullable double
  "fees_currency": "USD"            // nullable string
}
```

Always populates `fx_rates` (broker direct + inverse branches).

---

## `event_type = "OPTION_EXERCISE"`

A long option position exercised. `source_id` = a stable per-event id
(`<account>-<canonical_occ>-exercise-<event_ts>`); `instrument_id` = the
option contract (canonical OCC). The kernel closes the option lot at the
strike and credits / debits cash; the underlying-share delivery arrives
as a separate `TRADE` row in the same broker statement.

```json
{
  "underlying_id":    "BMY",         // string: the equity instrument the delivery references
  "settlement_price": 45.0,          // double: = strike (carried explicitly so corrections that adjust the assigned strike don't require an instruments row update)
  "delivered_qty":    100.0,         // double: underlying shares (or cash-equivalent units) moved; always positive, direction is in `delivered_side`
  "delivered_side":   "buy",         // string: "buy" | "sell" — from the portfolio's perspective
  "currency":         "USD"          // string: contract's trade currency
}
```

No FX field on v3 launch: IBKR exercise rows arrive in the option's trade
currency and the streaming ASOF over `fx_rates` (other events' broker
rates) covers FX. Add `fx_rate_to_base` when a broker reports one.

---

## `event_type = "OPTION_ASSIGNMENT"`

Symmetric to `OPTION_EXERCISE`, on the short side — the writer is
assigned. Same payload shape:

```json
{
  "underlying_id":    "BMY",
  "settlement_price": 45.0,
  "delivered_qty":    100.0,
  "delivered_side":   "buy",         // assigned short put → portfolio buys at strike
  "currency":         "USD"
}
```

---

## `event_type = "OPTION_EXPIRY"`

The contract reached expiry without exercise / assignment (out-of-the-
money or holder let it lapse). The kernel closes the open lot at price 0
on `business_ts`; no equity delivery.

```json
{
  "currency": "USD"                  // string: contract's trade currency, kept for symmetry
}
```

---

## `event_type = "TRANSFER_IN"`

The initial in-flight of an existing lot when the portfolio is created from
a pre-existing holding (typically a one-shot per-lot at portfolio bootstrap).
`source_id` = `lot_id`; `instrument_id` = the held instrument;
`business_ts` = `acquisition_date` (NOT the recording timestamp).

```json
{
  "transfer_id":             "T-2024-01-15-batch-1",  // the umbrella transfer event id (multiple lot events share this)
  "quantity":                10.0,
  "cost_basis_native":       1500.0,                  // original cost basis in `currency`
  "currency":                "USD",
  "acquisition_date":        1705276800000000,        // microseconds UTC — also = envelope.business_ts
  "fx_rate_at_acquisition":  0.91,                    // nullable double — primary FX field for transfer-in IRR/cost
  "fx_rate_to_base":         0.92                     // nullable double — legacy/audit; NOT used for IRR or cost basis when fx_rate_at_acquisition is set
}
```

`fx_rate_at_acquisition` (not `fx_rate_to_base`) is the primary FX field for
transfer-ins. The latter is carried for ledger reconciliation only.

---

## Validation

The kernel's `services/udf-server/marshalling.py` validates payload shapes at
fold time. Missing required fields cause the event to be skipped with a
structured log. Field-name typos surface there, not at the broker.

Producer-side tests in `services/reference-admin/test/test_api.py` should
cover at least one happy-path publish per event_type asserting payload-shape
compliance.

---

## Evolution

Adding a new field: optional fields can be added at any time without coordination
(consumers tolerate unknown fields).

Adding a new event_type: extend the Avro `EventType` enum in
`schemas/portfolio_events.v1.avsc`, document the new payload shape here,
extend `marshalling.py` to dispatch on it. See
`docs/risingwave/v2/03-extension-guide.md`.

Renaming a field: don't. Add a new field and migrate consumers / kernel to
read the new one; deprecate the old one over a soak period.
