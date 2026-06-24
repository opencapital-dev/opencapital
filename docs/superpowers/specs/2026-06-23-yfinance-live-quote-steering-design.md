# yfinance live-quote steering — design

Date: 2026-06-23
Status: draft (pending verification gate + review)
Repo: `oc-plugin-yfinance-app` (sibling of `opencapital`)

## Problem

After changing a ticker, a wrong live quote lands under an instrument and inflates NAV
(e.g. `AET` → London `AET.L` ≈ £0.62, but a quote arrived `venue=NCM, USD, 3.21` from the
US listing → equity 77k→114k). It recurs specifically on **ticker change**, and can corrupt
instruments the user did not touch.

## Root cause (evidence-backed)

1. **Wrong-listing snapshots from ambiguous symbols.** Yahoo's REST (backfill) and ws (live)
   resolve a **bare/ambiguous** symbol differently. `AET` has 106 GBP/London OHLCV bars (REST
   → London) but its bad live quote was `NCM/USD` (ws → US). Fully-qualified symbols
   (`AET.L`, `CPKR.TO`) observed resolving consistently (correct venue).
2. **Full re-subscribe amplifies it.** `go-yfinance`'s `WebSocket.Subscribe` re-sends the
   **entire** accumulated subscription list every call (`websocket.go` `getSubscriptionList()`),
   and Yahoo answers with a fresh **snapshot** (last trade) for every symbol. So changing one
   ticker re-snapshots all ~19 symbols; any ambiguous symbol in the set re-emits its
   wrong-listing snapshot. In steady state nobody calls `Subscribe`, so no snapshots — which is
   why the corruption is tied to ticker changes. (`Unsubscribe` is incremental; only `Subscribe`
   is full-list — asymmetric.)
3. **Valuation is currency-blind (pre-existing).** `portfolio_per_tick` values a position with
   the raw price number and the *position's* currency, ignoring the price row's currency — so a
   foreign-listing price (3.21) is spent as 3.21 GBP. This is the amplifier that turns a bad
   quote into a NAV spike, but is explicitly out of scope here (see below).

Not the cause: Yahoo sending garbage for a *correct* symbol. Correctly-qualified symbols stream
correct venues (`CHTR→NMS`, `ENPH→NGM`, `HON→NMS`, `CPKR→TOR` all observed correct). The
separate ws quirk where CPKR's *currency label* flips CAD↔USD with a correct price/venue is
**harmless** (valuation ignores the label) and is not addressed here.

## Goal

**Steer the feed to the right listing — do not post-hoc drop quotes.** Ensure the symbol we
subscribe is unambiguous so the ws delivers the correct listing, and stop a single ticker change
from re-snapshotting unrelated instruments.

## Verification gate (must pass before implementation is finalized)

Re-run the ws probe **during London market hours** subscribing only `AET.L`:
- **If its snapshot is `LSE/GBP`** → qualified symbols resolve correctly → steering is
  sufficient; proceed with the design below.
- **If its snapshot is `NCM/USD`** → even a qualified symbol resolves wrong on the ws → steering
  alone cannot fix it; STOP and re-open the design (a venue-consistency safety net would be
  required despite the "no drop" preference). Record the finding.

This gate exists because the "no drop" approach is only valid if qualified symbols are reliable,
which current evidence strongly suggests but could not directly confirm (market closed).

## Design

### Component 1 — Symbol qualification at mapping time (correctness; primary)

When a symbol is set/backfilled, resolve it to Yahoo's canonical fully-qualified symbol and
persist it; subscribe and backfill that canonical form so REST and ws cannot disagree.

- In `yfclient.go`, `FetchBars` already reads `GetHistoryMetadata()` (currency). Extend it to
  also return the metadata's resolved **`Symbol`** and **`Exchange`**.
- In `runBackfillJob`, write the resolved `{symbol, exchange}` into the mapping's `vendor_meta`
  (e.g. `vendor_meta.canonical = {symbol, exch}`). If the resolved symbol differs from the
  stored mapping symbol (the input was bare/ambiguous), update the mapping symbol to the
  canonical one so subsequent subscribes use it.
- `live.go SetSymbols` already receives full `TickerMapping`s — it should subscribe the
  **canonical** symbol when present, falling back to the raw symbol until the first backfill
  resolves it.

Effect: a bare `AET` is upgraded to `AET.L` (whatever REST resolved), and the ws then
subscribes the same listing REST uses → consistent → correct snapshot.

### Component 2 — Incremental subscribe (blast-radius + load; secondary)

Re-subscribe only the changed symbol so one ticker change does not re-snapshot all instruments.

- `go-yfinance` exposes no partial subscribe (only the full-list `Subscribe`). Decision (to be
  confirmed in the plan): **vendor a thin fork** of the live client adding
  `SubscribeOnly(symbols)` that writes `{"subscribe":[symbols]}` directly, OR keep upstream and
  accept full re-subscribe (relying on Component 1 so every re-snapshot is correct).
- This is an optimization/hardening, NOT the correctness fix. If Component 1 holds (all
  subscribed symbols are unambiguous), full re-subscribe is harmless. Recommend implementing
  Component 1 first; add Component 2 only if churn/load warrants.

### Kept as-is

- **purge-on-remap** (0.1.4) stays — it clears stale data on a genuine symbol change.
- Earlier manual data cleanup remains a one-off for historical rows.

## Out of scope (explicit)

- **Dropping/quarantining quotes** by currency or venue — the user wants steering, not filtering.
- **Data-plane currency-aware valuation** — real latent correctness gap, but not this fix.
- **CPKR currency-label flip** — harmless ws quirk; ignored.

## Testing

- Unit: `FetchBars` returns resolved `{symbol, exchange}`; mapping write of `vendor_meta.canonical`.
- Unit: `SetSymbols` prefers the canonical symbol over the raw input.
- Unit (if Component 2): `SubscribeOnly` emits `{"subscribe":[delta]}` only.
- Manual: at London hours, change a ticker and confirm only the changed instrument re-snapshots
  and `AET.L` ingests `LSE/GBP`.

## Open questions

1. Verification gate result (above) — determines whether steer-only is viable.
2. Component 2 fork vs upstream — decide in the implementation plan.
