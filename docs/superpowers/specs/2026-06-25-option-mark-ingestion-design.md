# Option-mark ingestion (Yahoo option chains) — design

**Date:** 2026-06-25
**Status:** design, pending review
**Implementation target:** `oc-plugin-yfinance-app` (basic-data plugin) — backend
ingestion + frontend (nav restructure, Instruments Stocks/Options tabs, Overview
landing, option-poll settings). No `opencapital` dataplane change.

## Problem

Option positions are marked-to-market via the `option_marks` materialized view
(`dataplane/risingwave/schemas/03-unifying-views/option_marks.sql`), which reads
`data_log` filtered to `source_namespace = 'prices.option_mark'`
(payload `{"close": <double>, "currency": "USD"}`). The metrics
`instrument_per_tick` / `portfolio_per_tick` ASOF-join it: for `kind = 'option'`
the MtM uses `om.close` (× `contract_multiplier`); for everything else `px.price`.

Today the **only producer** of `prices.option_mark` is the broker-statement
upload path (one row per Open Positions option entry). Marks are therefore a
stale snapshot frozen at statement-upload time. Equity prices, by contrast, are
auto-fed by the basic-data plugin (`prices.ohlcv` / `prices.quote`).

**Goal:** auto-feed `prices.option_mark` for currently-held option contracts by
polling Yahoo option chains, accumulating a forward time series (one row per
held contract per poll). Free, no API key, ~15-min-delayed — acceptable.

## Key constraint

Yahoo's option-chain endpoint (`/v7/finance/options/{underlying}`) returns the
**current snapshot only** — no historical option-price bars. We cannot backfill
option price history. The series is built **forward** from first poll. This rules
out reusing the equity backfill job queue (which is start/end-bounded history).

## Library

No new dependency. `github.com/wnjoon/go-yfinance v1.3.0` — already vendored in the
plugin — exposes options:

- `Ticker.Options() ([]time.Time, error)` — expiry dates
- `Ticker.OptionChain(date string) (*models.OptionChain, error)`
- `Ticker.OptionChainAtExpiry(date time.Time) (*models.OptionChain, error)`

`models.Option` carries: `ContractSymbol`, `Strike`, `Bid`, `Ask`, `LastPrice`,
`Currency`, `ImpliedVolatility`, `Volume`, `OpenInterest`, `Expiration`,
`InTheMoney`. `OptionChain.Underlying` (`*OptionQuote`) carries `MarketState`,
`Currency`, `RegularMarketTime`.

## Identity & mapping

Option `instrument_id` canonical format (mirror of
`oc-plugin-core-app/src/lib/import/occ.ts`):

```
{UNDERLYING} {DD}{MON}{YY} {STRIKE} {C|P}      e.g.  AAPL 17JAN25 150 C
```

This is **not** Yahoo's `contractSymbol` (`AAPL250117C00150000`). Mapping path:

1. Parse the OCC id → `(underlying_root, expiry_date, strike, right)`.
2. Resolve `underlying_root` → Yahoo underlying symbol via the underlying-symbol
   map (see below).
3. `FetchOptionChain(yahoo_symbol, expiry_date)` → calls + puts.
4. Match the row by **strike + right** (robust; avoids Yahoo `contractSymbol`
   padding rules and adjusted-root quirks).

### Underlying-symbol map (required in v1)

Held options can have underlyings that are not plain US tickers (index options,
non-US, post-split adjusted). v1 therefore makes underlying resolution explicit
and user-editable, reusing the existing mapping store
`basic_data.instrument_ticker_mapping` (keyed `(instrument_id, portfolio_id)` →
`symbol`, `subscribed`, `vendor_meta`, auto-seeded on discovery):

- For each held option, derive its `underlying_root`.
- **Lazy auto-seed (in the poll loop, not at init):** on first sight of a root
  with no mapping row, insert one keyed `(underlying_root, portfolio_id)` with
  `symbol = underlying_root` (default), `subscribed = true`, and
  `vendor_meta.kind = "option_underlying"`, then use it the same tick. So plain
  US-ticker underlyings (`AAPL → AAPL`) work out of the box with no manual step;
  the map is the **override** for the cases that need it.
- The user edits these in the **Instruments → Options tab** (rows filtered by
  `vendor_meta.kind = "option_underlying"`) to override, e.g. `SPX → ^SPX`,
  `RUT → ^RUT`. See *Frontend* below.
- A root whose mapping is explicitly **unsubscribed or blanked** is skipped
  (logged once), same posture as unmapped equities.

*Considered alternative:* a dedicated `option_underlying_mapping` table + handler
+ UI section. Rejected for v1 — duplicates plumbing; the `vendor_meta.kind`
marker on the existing table gives first-class behaviour with less surface.

## Components

### Backend (`oc-plugin-yfinance-app/pkg/plugin`)

| File | New/Edit | Responsibility |
|------|----------|----------------|
| `occ.go` | new | `ParseOcc(id) (OccParts, error)` — Go mirror of `occ.ts` canonical format. Pure, unit-tested against shared fixtures. |
| `optionchain.go` | new | `YfClient.FetchOptionChain(ctx, underlyingSymbol string, expiry time.Time) (*OptionChainResult, error)` — wraps `yfticker.OptionChainAtExpiry`, takes a limiter token (like `FetchBars`), returns matched rows + underlying `MarketState`/`Currency`/quote-time. |
| `option_poll.go` | new | `StartOptionPollLoop(ctx, client, app, ...)` — ticker at the configured interval; load held options, group by `(yahooSymbol, expiry)`, fetch once per group, match held strikes, publish. Mirrors `StartDiscoveryLoop` shape. Lazily seeds `option_underlying` map rows. Honors enable/interval from settings. |
| `handlers_option_underlyings.go` | new | `GET /yf/option-underlyings` (list `option_underlying` rows + rollup: held-contract count, last-poll ts, last-mark age, per `(root, portfolio)`), `POST` to upsert symbol / toggle subscribe. Backs the Options tab. |
| `handlers_overview.go` | new | `GET /yf/overview` — counts (held equities/options), last option-poll ts, mapping coverage / health rollup. Backs the Overview page. |
| `handlers_settings.go` | edit | add `optionPollEnable` + `optionPollIntervalSec` to the GET/PUT settings payload. |
| `publish.go` | edit | add `OptionMarkNamespace = "prices.option_mark"`. |
| `routing.go` | edit | register the new resource routes. |
| `app.go` | edit | start the option-poll loop on plugin init (alongside discovery/backfill/live), gated on `optionPollEnable`; thread `optionPollIntervalSec` (default 900). |

### Frontend (`oc-plugin-yfinance-app/src`)

| File | New/Edit | Responsibility |
|------|----------|----------------|
| `plugin.json` | edit | Nav: add **Overview** page `defaultNav: true`; demote **Instruments** (was "Tickers", remove `defaultNav`); keep **Settings**. |
| `constants.ts` | edit | `ROUTES` — add `Overview`; rename `Tickers → Instruments`. |
| `components/App/App.tsx` | edit | add Overview route; default redirect → Overview (was Tickers). |
| `components/Page.tsx` | edit | padded, max-width, vertical-rhythm content shell (`Page.Contents`/`PageShell`) — fixes the no-margin/no-section look. Every page uses it. |
| `pages/OverviewPage.tsx` | new | Landing: held equity/option counts, last poll time, mark/backfill health from `/yf/overview`. |
| `pages/InstrumentsPage.tsx` | new (refactor of `TickersPage.tsx`) | `TabsBar` with **Stocks** + **Options** tabs. |
| `components/instruments/StocksTab.tsx` | new (extract) | The existing equity operator table, lifted verbatim out of `TickersPage`. |
| `components/instruments/OptionsTab.tsx` | new | Table of `option_underlying` rows per `(root, portfolio)`: editable Yahoo symbol (reuses the symbol `Select`/lookup), subscribe toggle, held-contract count, last-mark recency. |
| `api/options.ts` | new | `listOptionUnderlyings()`, `setOptionUnderlyingSymbol()`, `toggleOptionUnderlying()`. |
| `api/overview.ts` | new | `getOverview()`. |
| `api/settings.ts` | edit | add `optionPollEnable`, `optionPollIntervalSec` to `Settings`. |
| `pages/SettingsPage.tsx` | edit | use `Page` shell; group into sections (**API keys**, **Option polling**); add option-poll enable switch + interval input (writes via existing `putSettings`). |

## Data flow (per poll tick)

```
heldPairs() WHERE kind = 'option'                  # instruments_catalog already exposes kind
  → ParseOcc(instrument_id) → (root, expiry, strike, right)
  → resolve root → yahooSymbol via option_underlying mapping
       (no row → seed default symbol=root,subscribed=true and use it; unsubscribed/blank → skip)
  → group held contracts by (yahooSymbol, expiry)  # one Yahoo call covers all strikes
  → for each group:
       chain = FetchOptionChain(yahooSymbol, expiry)   # limiter token
       if chain.Underlying.MarketState != "REGULAR": skip group   # market-hours gate
       for each held (strike, right) in group:
           row = match in chain.calls/puts by (strike, right)      # skip if absent
           mark = (row.Bid > 0 && row.Ask > 0) ? (row.Bid + row.Ask) / 2 : row.LastPrice
           if mark <= 0: skip                                       # no usable price
           ccy = firstNonEmpty(row.Currency, chain.Underlying.Currency, "USD")
           observedUs = chain.Underlying.RegularMarketTime (µs) or now
           INSERT INTO data_log
             (source_namespace, source_id, portfolio_id, observed_at, ingest_ts,
              source, plugin_id, trace_id, payload, rw_key)
           VALUES
             ('prices.option_mark', <occId>, <portfolioId>, observedUs, now,
              'yfinance', pluginID, traceId,
              {"close": mark, "currency": ccy},
              datakey.DataKey(pluginID, OptionMarkNamespace, portfolioId, occId, observedUs))
```

Downstream is unchanged: `option_marks` MV consumes the new rows; `instrument_per_tick`
ASOF-joins and multiplies by `contract_multiplier` (100 for `REGULAR`-size contracts).

### Semantics

- **Mark = mid, fallback to last.** `(bid+ask)/2` when both quotes > 0, else
  `lastPrice`. Fair value that survives illiquid contracts with no live quote.
- **Per-share premium.** Yahoo quotes option premium per share; `contract_multiplier`
  applies downstream — matches existing statement-mark semantics. (Assumption to
  confirm against a real statement-mark row: statement `close` is also per-share.)
- **Statement + Yahoo coexist.** Statement-upload marks are left untouched; Yahoo
  polls add more rows to the same namespace. The MtM ASOF join already takes the
  latest mark ≤ tick, so the freshest source wins automatically. No coordination.
- **Accumulate, never delete.** Option marks are history. The equity stale-DELETE
  in `rw_repo.go` (`source_namespace IN ('prices.ohlcv','prices.quote')`) is **not**
  extended to `prices.option_mark`.

## Edge cases

- `bid = ask = 0` and `lastPrice = 0` → skip the contract this tick; ASOF falls back
  to the prior mark.
- Expired contract (`expiry < today`) → skip; Yahoo has nothing.
- Underlying root mapping explicitly unsubscribed/blanked → skip group, log once.
  (First-sight roots auto-seed to `symbol=root,subscribed=true`, so they are not skipped.)
- Market closed (`MarketState != REGULAR`) → skip group; no row (keeps the series to
  market hours, matches the chosen cadence).
- go-yfinance / network error for a group → log, continue other groups; retried next tick.

## Cadence (settings-driven)

Ticker interval = `optionPollIntervalSec` (default **900** = 15 min), gated to
market hours via the chain's underlying `MarketState` (no separate exchange
calendar needed). At the default, ~26 rows/contract/day. Independent of the
equity discovery/backfill cadence. The whole loop is gated on
`optionPollEnable` (default **true**).

Both live in the existing settings plumbing (`Settings` type → `handlers_settings.go`
→ `app.go` options), edited in **Settings → Option polling**. Changing the
interval/enable takes effect on the loop's next tick (or restart of the loop on
toggle), matching how the equity poll interval already behaves.

## Frontend / nav / settings

- **Nav restructure.** `plugin.json` gains an **Overview** page (`defaultNav: true`);
  the former "Tickers" page becomes **Instruments** (no `defaultNav`); **Settings**
  unchanged. `App.tsx` default redirect → Overview.
- **Overview** (landing): held equity/option counts, last option-poll timestamp,
  mark + backfill health — from `GET /yf/overview`.
- **Instruments** = `TabsBar` with two tabs:
  - **Stocks** — the existing equity operator table (extracted verbatim from
    `TickersPage`), behaviour unchanged.
  - **Options** — one row per `(underlying_root, portfolio)` from the
    `option_underlying` mappings: editable Yahoo symbol (reuses the symbol
    lookup/`Select`), subscribe toggle, held-contract count, last-mark recency.
- **Settings** — adds an **Option polling** group: enable switch +
  interval input, persisted through the existing `putSettings`. (FRED key
  control unchanged.)

### UI quality / layout shell

Current pages look unfinished: `SettingsPage` renders a bare `<div>` (it doesn't
even use `Page`), and `Page` wraps `PluginPage` with **no padding, no max-width,
no section grouping** — so content is flush to the viewport edge with no margins
or visual sections (see the reported Settings screenshot).

Fix as part of this work, applied to **every** page (Overview, Instruments, Settings):

- **Padded content shell.** Give `Page.Contents` (or a new `PageShell`) real
  padding, a sensible `max-width`, and consistent vertical rhythm between blocks.
- **Sectioned layout.** Group related controls into titled sections
  (`FieldSet` / `Card`), not a flat stack of fields. Settings → at least
  **API keys** (FRED) and **Option polling** sections.
- **Consistent page header.** Title + short description per page via the shell,
  not ad-hoc `<h2>`.
- Use the `frontend-design` skill during execution to make the new + reworked
  pages visually coherent (hierarchy, spacing, states), not just functional.

Plan-execution note: invoke **`/frontend-design:frontend-design`** when building
each user-facing page so layout, spacing, and states are designed deliberately.

## Testing

- `occ_test.go` — parse canonical ids; round-trip and reject malformed; shared
  fixtures cross-checked against `occ.ts` outputs.
- `option_poll_test.go` (go-yfinance mocked):
  - mark selection: mid when both quotes present; last when a side is 0; skip when
    no usable price.
  - strike + right matching within calls/puts.
  - market-state gate (skip when not `REGULAR`).
  - grouping: N held strikes on one `(underlying, expiry)` → exactly one chain fetch.
  - payload shape + `rw_key` derivation (`datakey.DataKey`) + `observed_at` source.
  - underlying resolution via subscribed `option_underlying` mapping; skip when absent.
  - interval/enable honored from settings (loop off when `optionPollEnable=false`).
- `handlers_option_underlyings` / `handlers_settings` Go handler tests — list/upsert,
  subscribe toggle, settings round-trip of the two new fields.
- Frontend (jest): `InstrumentsPage` renders both tabs and switches; `OptionsTab`
  edits symbol + toggles subscribe; `SettingsPage` shows + saves option-poll fields;
  `App` default route lands on Overview. Mirrors existing `App.test.tsx` patterns.

## Out of scope (v1)

- Historical option-price backfill (Yahoo free cannot provide it).
- Greeks / IV persistence (available in the payload; could be added to the
  `option_mark` payload later without a contract break).
- Live (WebSocket) option ticks — Yahoo's live stream does carry option pricing
  fields, but interval REST polling is sufficient and simpler for v1.
- Overview page is a read-only status rollup in v1 (no actions/controls beyond nav).
