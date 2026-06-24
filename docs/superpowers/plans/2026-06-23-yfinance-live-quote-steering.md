# yfinance live-quote steering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop wrong-listing live quotes from corrupting NAV after a ticker change, by subscribing the ws to the same canonical symbol REST resolved (steering, not dropping).

**Architecture:** At backfill, capture Yahoo's REST-resolved canonical `{symbol, exchange}` and store it in the mapping's `vendor_meta`. The live subscriber subscribes that canonical symbol instead of the raw user input, so REST and the ws resolve the same listing. A verification gate confirms qualified symbols resolve correctly before this is trusted; an optional incremental-subscribe follow-up reduces re-snapshot blast radius.

**Tech Stack:** Go 1.26, `github.com/wnjoon/go-yfinance@v1.3.0` (REST `ticker` + `live` ws), pg8000/pgwire to RisingWave + control PG, `grafana-plugin-sdk-go`.

## Global Constraints

- Repo: `~/trading-code/oc-plugin-yfinance-app` (sibling of `opencapital`). Branch from its `main`.
- Tests run with `go test ./pkg/plugin/` from the repo root; vet with `go vet ./pkg/plugin/`.
- `vendor_meta` is JSONB stored via `a.client.PGExec` to the control PG; `data_log`/MVs are RisingWave via `a.client.Exec`/`Query`.
- go-yfinance types (verbatim): REST `models.ChartMeta{ Currency string; Symbol string; ExchangeName string }` from `ticker.GetHistoryMetadata()`; live `models.PricingData{ ID string; Price float32; Currency string; Exchange string }`.
- Ship per direct-to-production flow: tag `v*` → publish action pushes directly to `opencapital-dev/plugins` and bumps `versions[]` in the plugin repo's `oc-plugin.json`. (Not part of this plan's tasks; release after merge.)
- Do NOT drop/quarantine quotes; do NOT change the data-plane valuation; ignore the harmless CPKR currency-label flip.

---

### Task 1: Verification gate — confirm a qualified symbol snapshots correctly

**This gate decides whether the rest of the plan is valid. It must be run while the London market is open (≈08:00–16:30 UK).**

**Files:**
- Create (throwaway): `wscheck/main.go` (delete after)

- [ ] **Step 1: Write the probe**

```go
package main

import (
	"fmt"
	"time"

	yflive "github.com/wnjoon/go-yfinance/pkg/live"
	yfmodels "github.com/wnjoon/go-yfinance/pkg/models"
)

func main() {
	ws, err := yflive.New()
	if err != nil {
		panic(err)
	}
	defer ws.Close()
	if err := ws.Connect(); err != nil {
		panic(err)
	}
	_ = ws.Listen(func(d *yfmodels.PricingData) {
		fmt.Printf("QUOTE id=%s exch=%s ccy=%s price=%.4f\n", d.ID, d.Exchange, d.Currency, d.Price)
	})
	_ = ws.Subscribe([]string{"AET.L"})
	time.Sleep(20 * time.Second)
}
```

Note: use blocking `Listen` in a goroutine if `Listen` blocks `Subscribe`; here `Subscribe` is called after `Listen` returns the handler is registered async — if no quotes appear, move `ws.Subscribe` before `ws.Listen` and run `Listen` in a `go func()`.

- [ ] **Step 2: Run it during London hours**

Run: `cd ~/trading-code/oc-plugin-yfinance-app && timeout 30 go run ./wscheck`
Expected: at least one `QUOTE id=AET.L ...` line.

- [ ] **Step 3: Decide**

- If `exch=LSE` / `ccy=GBp|GBP` → **PASS**: qualified symbols resolve correctly. Continue to Task 2.
- If `exch=NCM|NYQ|...` / `ccy=USD` → **FAIL**: even a qualified symbol resolves to the wrong listing on the ws. STOP. Do not implement Tasks 2–5. Record the finding in the spec and re-open design (a venue-consistency safety net would be required despite the "no drop" preference).

- [ ] **Step 4: Clean up**

Run: `rm -rf ~/trading-code/oc-plugin-yfinance-app/wscheck`

---

### Task 2: `canonicalSymbol` helper

**Files:**
- Modify: `pkg/plugin/live.go` (add helper near `SetSymbols`)
- Test: `pkg/plugin/live_test.go`

**Interfaces:**
- Produces: `func canonicalSymbol(m TickerMapping) string` — returns `vendor_meta.canonical.symbol` when present/non-empty, else `m.Symbol`.

- [ ] **Step 1: Write the failing test**

```go
func TestCanonicalSymbol(t *testing.T) {
	// No canonical → raw symbol.
	if got := canonicalSymbol(TickerMapping{Symbol: "AET", VendorMeta: map[string]any{}}); got != "AET" {
		t.Errorf("no-canonical = %q, want AET", got)
	}
	// Canonical present → canonical wins.
	m := TickerMapping{Symbol: "AET", VendorMeta: map[string]any{
		"canonical": map[string]any{"symbol": "AET.L", "exch": "LSE"},
	}}
	if got := canonicalSymbol(m); got != "AET.L" {
		t.Errorf("canonical = %q, want AET.L", got)
	}
	// Empty canonical symbol → fall back to raw.
	m2 := TickerMapping{Symbol: "AET", VendorMeta: map[string]any{
		"canonical": map[string]any{"symbol": "", "exch": "LSE"},
	}}
	if got := canonicalSymbol(m2); got != "AET" {
		t.Errorf("empty-canonical = %q, want AET", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/plugin/ -run TestCanonicalSymbol -v`
Expected: FAIL — `undefined: canonicalSymbol`.

- [ ] **Step 3: Implement**

```go
// canonicalSymbol returns the REST-resolved fully-qualified Yahoo symbol stored
// at backfill in vendor_meta.canonical.symbol, falling back to the raw mapping
// symbol until the first backfill resolves it. Subscribing the canonical form
// makes the live ws resolve the same listing REST used.
func canonicalSymbol(m TickerMapping) string {
	if c, ok := m.VendorMeta["canonical"].(map[string]any); ok {
		if s, ok := c["symbol"].(string); ok && s != "" {
			return s
		}
	}
	return m.Symbol
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./pkg/plugin/ -run TestCanonicalSymbol -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/live.go pkg/plugin/live_test.go
git commit -m "feat(live): canonicalSymbol helper (vendor_meta.canonical → raw fallback)"
```

---

### Task 3: `SetCanonicalIdentity` repo method

**Files:**
- Modify: `pkg/plugin/pg_repo.go`
- Test: `pkg/plugin/pg_repo_test.go`

**Interfaces:**
- Consumes: `TickerMapping`, `GetTickerMapping`, `nowMicros()` (existing).
- Produces: `func (a *App) SetCanonicalIdentity(ctx context.Context, instrumentID, portfolioID, symbol, exchange string) error` — merges `{symbol, exch}` under `vendor_meta.canonical` and writes it back; no-op if `symbol == ""`.

- [ ] **Step 1: Write the failing test**

```go
func TestSetCanonicalIdentitySQL(t *testing.T) {
	fc := &fakeClient{pgQueryResult: mappingResult("AET.L")}
	app := makeAppWithFakeClient(fc)
	if err := app.SetCanonicalIdentity(context.Background(), "instr-1", "port-1", "AET.L", "LSE"); err != nil {
		t.Fatalf("SetCanonicalIdentity: %v", err)
	}
	// One UPDATE PGExec writing vendor_meta with the canonical block.
	if len(fc.pgExecCalls) != 1 {
		t.Fatalf("expected 1 PGExec, got %d", len(fc.pgExecCalls))
	}
	sql := fc.pgExecCalls[0].sql
	if !strings.Contains(sql, "instrument_ticker_mapping") || !strings.Contains(sql, "vendor_meta") {
		t.Errorf("SQL missing table/vendor_meta: %s", sql)
	}
	// The marshalled vendor_meta arg must carry canonical.symbol = AET.L.
	var found bool
	for _, a := range fc.pgExecCalls[0].args {
		if s, ok := a.(string); ok && strings.Contains(s, `"canonical"`) && strings.Contains(s, `"AET.L"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("vendor_meta arg missing canonical AET.L: %v", fc.pgExecCalls[0].args)
	}
}

func TestSetCanonicalIdentityNoopOnEmpty(t *testing.T) {
	fc := &fakeClient{pgQueryResult: mappingResult("AET.L")}
	app := makeAppWithFakeClient(fc)
	if err := app.SetCanonicalIdentity(context.Background(), "instr-1", "port-1", "", "LSE"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(fc.pgExecCalls) != 0 {
		t.Fatalf("empty symbol must be a no-op, got %d PGExec", len(fc.pgExecCalls))
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/plugin/ -run TestSetCanonicalIdentity -v`
Expected: FAIL — `undefined: SetCanonicalIdentity`.

- [ ] **Step 3: Implement** (model on existing `SetClassification`)

```go
// SetCanonicalIdentity records the REST-resolved canonical Yahoo identity for an
// instrument under vendor_meta.canonical = {symbol, exch}. The live subscriber
// reads symbol via canonicalSymbol() so it subscribes the same listing REST
// resolved. Best-effort; a no-op when symbol is empty.
func (a *App) SetCanonicalIdentity(ctx context.Context, instrumentID, portfolioID, symbol, exchange string) error {
	if symbol == "" {
		return nil
	}
	cur, err := a.GetTickerMapping(ctx, instrumentID, portfolioID)
	if err != nil {
		return err
	}
	meta := cur.VendorMeta
	if meta == nil {
		meta = map[string]any{}
	}
	meta["canonical"] = map[string]any{
		"symbol": symbol,
		"exch":   strings.ToUpper(strings.TrimSpace(exchange)),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal vendor_meta: %w", err)
	}
	_, err = a.client.PGExec(ctx, `
		UPDATE yfinance.instrument_ticker_mapping
		   SET vendor_meta = $1::jsonb,
		       updated_at  = $2
		 WHERE instrument_id = $3
		   AND portfolio_id  = $4
	`, string(metaJSON), nowMicros(), instrumentID, portfolioID)
	if err != nil {
		return fmt.Errorf("set canonical identity: %w", err)
	}
	return nil
}
```

Add `"strings"` to `pg_repo.go` imports if not present.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./pkg/plugin/ -run TestSetCanonicalIdentity -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/pg_repo.go pkg/plugin/pg_repo_test.go
git commit -m "feat(repo): SetCanonicalIdentity writes vendor_meta.canonical"
```

---

### Task 4: Capture canonical identity at backfill

**Files:**
- Modify: `pkg/plugin/yfclient.go` (`FetchBars` returns resolved symbol + exchange)
- Modify: `pkg/plugin/backfill_worker.go` (call `SetCanonicalIdentity` after a successful fetch)
- Modify any other `FetchBars` callers to match the new signature.

**Interfaces:**
- Consumes: `models.ChartMeta{Symbol, ExchangeName, Currency}`, `App.SetCanonicalIdentity` (Task 3).
- Produces: `FetchBars(...) ([]yfmodels.Bar, string, string, string, float64, error)` returning `(bars, currency, resolvedSymbol, resolvedExchange, referencePrice, err)`.

- [ ] **Step 1: Extend `FetchBars` to return resolved symbol + exchange**

In `yfclient.go`, change the signature and the metadata extraction. Current:

```go
func (c *YfClient) FetchBars(ctx context.Context, symbol, barSize string, start, end time.Time) ([]yfmodels.Bar, string, float64, error) {
	...
	currency := ""
	if meta := t.GetHistoryMetadata(); meta != nil {
		currency = meta.Currency
	}
	...
	return bars, currency, referencePrice, nil
}
```

Change to:

```go
func (c *YfClient) FetchBars(ctx context.Context, symbol, barSize string, start, end time.Time) ([]yfmodels.Bar, string, string, string, float64, error) {
	...
	currency, resolvedSymbol, resolvedExchange := "", "", ""
	if meta := t.GetHistoryMetadata(); meta != nil {
		currency = meta.Currency
		resolvedSymbol = meta.Symbol
		resolvedExchange = meta.ExchangeName
	}
	...
	return bars, currency, resolvedSymbol, resolvedExchange, referencePrice, nil
}
```

(Apply the same two extra return values to every `return` inside `FetchBars`, including error returns: `return nil, "", "", "", 0, err`.)

- [ ] **Step 2: Update the backfill caller**

In `backfill_worker.go`, current:

```go
bars, rawCurrency, referencePrice, err := yf.FetchBars(ctx, symbol, job.BarSize, start, end)
```

Change to:

```go
bars, rawCurrency, resolvedSymbol, resolvedExchange, referencePrice, err := yf.FetchBars(ctx, symbol, job.BarSize, start, end)
```

Then, after the existing `state.MarkFinished(job, "done", ...)` success path (end of `runBackfillJob`), persist the canonical identity:

```go
	if cerr := app.SetCanonicalIdentity(ctx, job.InstrumentID, job.PortfolioID, resolvedSymbol, resolvedExchange); cerr != nil {
		log.DefaultLogger.Warn("set canonical identity failed",
			"instrument_id", job.InstrumentID, "portfolio_id", job.PortfolioID,
			"resolved_symbol", resolvedSymbol, "err", cerr)
	}
```

- [ ] **Step 3: Fix other callers + compile**

Run: `go build ./...`
Expected: build errors only at other `FetchBars` call sites (if any); update each to the 6-value form, then a clean build. Search first: `rg -n "FetchBars\(" pkg/`.

- [ ] **Step 4: Run the full suite**

Run: `go test ./pkg/plugin/ && go vet ./pkg/plugin/`
Expected: PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/yfclient.go pkg/plugin/backfill_worker.go
git commit -m "feat(backfill): capture REST canonical {symbol,exchange} into vendor_meta"
```

- [ ] **Step 6: Manual verification (no unit test — FetchBars hits the network)**

With the data plane + plugin running, change a ticker, wait for the backfill, then:
`psql -h localhost -p 5432 -U postgres -d control_db -c "SELECT instrument_id, symbol, vendor_meta->'canonical' FROM yfinance.instrument_ticker_mapping WHERE instrument_id='AET';"`
Expected: `vendor_meta->'canonical'` shows `{"symbol":"AET.L","exch":"LSE"}` (or the resolved values).

---

### Task 5: Live subscriber uses the canonical symbol

**Files:**
- Modify: `pkg/plugin/live.go` (`SetSymbols`)
- Test: `pkg/plugin/live_test.go`

**Interfaces:**
- Consumes: `canonicalSymbol` (Task 2).
- Produces: `SetSymbols` keys `desired`/`bySymbol` and subscribes by `canonicalSymbol(m)` instead of `m.Symbol`.

- [ ] **Step 1: Write the failing test for the selection**

```go
func TestSetSymbolsUsesCanonical(t *testing.T) {
	// Build the desired/bySymbol the same way SetSymbols does, asserting the
	// canonical symbol is the key. Extract the per-mapping symbol pick into a
	// loop over canonicalSymbol so this is unit-testable without a live ws.
	mappings := []TickerMapping{
		{InstrumentID: "AET", PortfolioID: "p", Symbol: "AET",
			VendorMeta: map[string]any{"canonical": map[string]any{"symbol": "AET.L", "exch": "LSE"}}},
	}
	got := desiredSymbols(mappings) // helper introduced below
	if _, ok := got["AET.L"]; !ok {
		t.Errorf("expected desired to contain canonical AET.L, got %v", got)
	}
	if _, ok := got["AET"]; ok {
		t.Errorf("must not subscribe the raw ambiguous symbol AET")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/plugin/ -run TestSetSymbolsUsesCanonical -v`
Expected: FAIL — `undefined: desiredSymbols`.

- [ ] **Step 3: Extract `desiredSymbols` and use it in `SetSymbols`**

Add helper:

```go
// desiredSymbols maps the upper-cased canonical Yahoo symbol → its targets.
// Subscribing the canonical form keeps the ws and REST on the same listing.
func desiredSymbols(mappings []TickerMapping) map[string][]symbolTarget {
	out := map[string][]symbolTarget{}
	for _, m := range mappings {
		sym := canonicalSymbol(m)
		if sym == "" {
			continue
		}
		up := strings.ToUpper(sym)
		out[up] = append(out[up], symbolTarget{InstrumentID: m.InstrumentID, PortfolioID: m.PortfolioID})
	}
	return out
}
```

In `SetSymbols`, replace the inline `desired`/`bySymbol` construction loop with:

```go
	bySymbol := desiredSymbols(mappings)
	desired := make(map[string]struct{}, len(bySymbol))
	for up := range bySymbol {
		desired[up] = struct{}{}
	}
```

(Keep the rest of `SetSymbols` — the `toAdd`/`toRemove` diff, `s.current = desired`, `s.bySymbol = bySymbol`, subscribe/unsubscribe — unchanged.)

- [ ] **Step 4: Run tests + vet**

Run: `go test ./pkg/plugin/ && go vet ./pkg/plugin/`
Expected: PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/live.go pkg/plugin/live_test.go
git commit -m "feat(live): subscribe canonical symbol so ws matches REST listing"
```

- [ ] **Step 6: Manual end-to-end**

Run the app at London hours. Change a ticker; confirm AET ingests an `LSE/GBP` quote (not `NCM/USD`) and no spike appears:
`psql -h localhost -p 4566 -U root -d dev -c "SELECT source_id, payload::jsonb->>'venue', payload::jsonb->>'currency' FROM data_log WHERE source_namespace='prices.quote' AND source_id='AET' ORDER BY ingest_ts DESC LIMIT 3;"`

---

### Task 6 (DEFERRED — optional): incremental subscribe

Per the spec, this is a load/blast-radius optimization, not the correctness fix. With Tasks 2–5 every subscribed symbol is canonical, so the full re-subscribe re-snapshots correct listings. Implement ONLY if re-snapshot churn proves to be a problem in practice.

If pursued: vendor a thin fork of `go-yfinance/pkg/live` adding `SubscribeOnly(symbols []string)` that writes `{"subscribe":[symbols]}` to the socket without `getSubscriptionList()`, and call it from `SetSymbols` for `toAdd` only. Decide fork-vs-replace at that time. Not specified further here (YAGNI).

---

## Notes

- Keep `purge-on-remap` (0.1.4) as-is.
- After Tasks 2–5 merge: release a new tag, `oras copy` staging→trusted, bump `plugins/yfinance-app.json` `versions[]` on `opencapital` `main` (see Global Constraints).
