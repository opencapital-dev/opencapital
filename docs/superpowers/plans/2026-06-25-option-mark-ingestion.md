# Option-Mark Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Auto-feed `prices.option_mark` for held option contracts by polling Yahoo option chains (forward, accumulating), and restructure the basic-data plugin UI (Overview landing + Instruments Stocks/Options tabs + option-poll settings) with a proper padded/sectioned layout.

**Architecture:** Purely additive in `oc-plugin-yfinance-app` (the basic-data plugin). A new poll loop loads held options from `instruments_catalog` (RW), groups by `(underlying, expiry)`, fetches each chain once via the already-vendored `go-yfinance` options API, matches each held contract by strike+right, and publishes a `prices.option_mark` row to `data_log` (RW). The existing `option_marks` MV + metrics consume it with **no opencapital dataplane change**. Underlying→Yahoo-symbol mapping reuses `basic_data.instrument_ticker_mapping` (Postgres) with a `vendor_meta.kind="option_underlying"` marker. Poll enable/interval live in `basic_data.app_settings` (Postgres), edited in the in-app Settings page.

**Tech Stack:** Go 1.26.3 (`grafana-plugin-sdk-go`, `github.com/wnjoon/go-yfinance v1.3.0`, `oc-plugin-sdk/datakey`, `oc-plugin-sdk/pluginclient`); React + TypeScript + `@grafana/ui` / `@grafana/runtime`; jest + Playwright. Reference spec: `opencapital/docs/superpowers/specs/2026-06-25-option-mark-ingestion-design.md`.

## Global Constraints

- **No new Go dependency.** Options come from the already-vendored `github.com/wnjoon/go-yfinance v1.3.0` (`Ticker.OptionChainAtExpiry`). Do not add `piquette/finance-go` or any other lib.
- **No opencapital dataplane change.** `option_marks` MV + `prices.option_mark` namespace already exist; this work only adds a producer.
- **Mark = mid, fallback to last:** `(bid+ask)/2` when both `> 0`, else `lastPrice`; skip the contract when the result is `<= 0`.
- **Per-share premium.** Publish the per-share option premium (Yahoo convention); `contract_multiplier` (100) is applied downstream — do not pre-multiply.
- **Accumulate, never delete.** Do not extend the equity stale-DELETE in `rw_repo.go` to `prices.option_mark`.
- **Market-hours gate:** skip a chain group when its underlying `MarketState != "REGULAR"`.
- **Poll defaults:** `option_poll_enable = true`, `option_poll_interval_sec = 900`.
- **RW vs Postgres client split:** `data_log` + `instruments_catalog` → `client.Exec` / `client.Query`. `basic_data.*` tables (`instrument_ticker_mapping`, `app_settings`) → `client.PGExec` / `client.PGQuery`.
- **grafanaDependency:** `>=12.3.0` (UI may use any `@grafana/ui` component available there).
- All new Go files: `package plugin` under `oc-plugin-yfinance-app/pkg/plugin`. All paths below are relative to `oc-plugin-yfinance-app/` unless noted.

---

## Phase A — Backend ingestion

### Task 1: OCC parser (`occ.go`)

Go mirror of `oc-plugin-core-app/src/lib/import/occ.ts`. Canonical option `instrument_id` format: `{UNDERLYING} {DD}{MON}{YY} {STRIKE} {C|P}` (e.g. `AAPL 17JAN25 150 C`).

**Files:**
- Create: `pkg/plugin/occ.go`
- Test: `pkg/plugin/occ_test.go`

**Interfaces:**
- Produces: `type OccParts struct { Underlying string; Expiry time.Time; Strike float64; Right string }` and `func ParseOcc(id string) (OccParts, error)`. `Right` is `"C"` or `"P"`. `Expiry` is UTC midnight of the expiry day.

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/occ_test.go
package plugin

import (
	"testing"
	"time"
)

func TestParseOcc(t *testing.T) {
	got, err := ParseOcc("AAPL 17JAN25 150 C")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Underlying != "AAPL" {
		t.Errorf("underlying = %q, want AAPL", got.Underlying)
	}
	want := time.Date(2025, 1, 17, 0, 0, 0, 0, time.UTC)
	if !got.Expiry.Equal(want) {
		t.Errorf("expiry = %v, want %v", got.Expiry, want)
	}
	if got.Strike != 150 {
		t.Errorf("strike = %v, want 150", got.Strike)
	}
	if got.Right != "C" {
		t.Errorf("right = %q, want C", got.Right)
	}
}

func TestParseOccFractionalStrikeAndPut(t *testing.T) {
	got, err := ParseOcc("spy 03MAR25 512.5 p")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Underlying != "SPY" || got.Strike != 512.5 || got.Right != "P" {
		t.Errorf("got %+v", got)
	}
}

func TestParseOccRejectsNonOption(t *testing.T) {
	if _, err := ParseOcc("AAPL"); err == nil {
		t.Fatal("expected error for non-OCC id")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/plugin/ -run TestParseOcc -v`
Expected: FAIL — `undefined: ParseOcc`.

- [ ] **Step 3: Write minimal implementation**

```go
// pkg/plugin/occ.go
package plugin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OccParts is the parsed canonical option instrument_id. Mirrors
// oc-plugin-core-app/src/lib/import/occ.ts:OccParts (the fields the chain
// lookup needs).
type OccParts struct {
	Underlying string
	Expiry     time.Time // UTC midnight of expiry day
	Strike     float64
	Right      string // "C" | "P"
}

var occMonths = map[string]time.Month{
	"JAN": time.January, "FEB": time.February, "MAR": time.March,
	"APR": time.April, "MAY": time.May, "JUN": time.June,
	"JUL": time.July, "AUG": time.August, "SEP": time.September,
	"OCT": time.October, "NOV": time.November, "DEC": time.December,
}

// occRe mirrors OCC_RE in occ.ts: {UND} {DD}{MON}{YY} {STRIKE} {C|P}.
var occRe = regexp.MustCompile(`^\s*([A-Z][A-Z0-9.]*)\s+(\d{1,2})([A-Z]{3})(\d{2})\s+(\d+(?:\.\d+)?)\s+([CP])\s*$`)

// ParseOcc parses a canonical option instrument_id. Returns an error for any
// string that is not in canonical OCC form (e.g. plain equity tickers).
func ParseOcc(id string) (OccParts, error) {
	m := occRe.FindStringSubmatch(strings.ToUpper(id))
	if m == nil {
		return OccParts{}, fmt.Errorf("not an OCC ticker: %q", id)
	}
	mon, ok := occMonths[m[3]]
	if !ok {
		return OccParts{}, fmt.Errorf("invalid OCC month %q in %q", m[3], id)
	}
	day, _ := strconv.Atoi(m[2])
	yy, _ := strconv.Atoi(m[4])
	strike, err := strconv.ParseFloat(m[5], 64)
	if err != nil {
		return OccParts{}, fmt.Errorf("invalid OCC strike in %q: %w", id, err)
	}
	expiry := time.Date(2000+yy, mon, day, 0, 0, 0, 0, time.UTC)
	if expiry.Day() != day || expiry.Month() != mon {
		return OccParts{}, fmt.Errorf("invalid OCC date in %q", id)
	}
	return OccParts{Underlying: m[1], Expiry: expiry, Strike: strike, Right: m[6]}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/plugin/ -run TestParseOcc -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/occ.go pkg/plugin/occ_test.go
git commit -m "feat(options): add Go OCC parser mirroring occ.ts"
```

---

### Task 2: Chain transform + mark selection (`optionchain.go`)

Pure helpers (testable, no network) plus the network `FetchOptionChain`. Strike+right matching and mark selection live in pure functions.

**Files:**
- Create: `pkg/plugin/optionchain.go`
- Test: `pkg/plugin/optionchain_test.go`

**Interfaces:**
- Consumes: `github.com/wnjoon/go-yfinance/pkg/models` (`models.Option`, `models.OptionChain`), `pkg/ticker`.
- Produces:
  - `type OptionRow struct { Strike float64; Right string; Bid, Ask, LastPrice float64; Currency string }`
  - `type OptionChainResult struct { Rows []OptionRow; MarketState, UnderlyingCurrency string; QuoteTimeUs int64 }`
  - `func optionRowsFromChain(calls, puts []yfmodels.Option) []OptionRow`
  - `func matchRow(rows []OptionRow, strike float64, right string) (OptionRow, bool)`
  - `func markFromRow(r OptionRow) (float64, bool)`
  - `func (c *YfClient) FetchOptionChain(ctx context.Context, underlyingSymbol string, expiry time.Time) (*OptionChainResult, error)`

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/optionchain_test.go
package plugin

import (
	"testing"

	yfmodels "github.com/wnjoon/go-yfinance/pkg/models"
)

func TestMarkFromRowMidWhenBothQuotes(t *testing.T) {
	mark, ok := markFromRow(OptionRow{Bid: 2.0, Ask: 2.4, LastPrice: 9})
	if !ok || mark != 2.2 {
		t.Fatalf("mark=%v ok=%v, want 2.2 true", mark, ok)
	}
}

func TestMarkFromRowFallsBackToLast(t *testing.T) {
	mark, ok := markFromRow(OptionRow{Bid: 0, Ask: 0, LastPrice: 3.1})
	if !ok || mark != 3.1 {
		t.Fatalf("mark=%v ok=%v, want 3.1 true", mark, ok)
	}
}

func TestMarkFromRowSkipsWhenNoPrice(t *testing.T) {
	if _, ok := markFromRow(OptionRow{Bid: 0, Ask: 0, LastPrice: 0}); ok {
		t.Fatal("expected ok=false when no usable price")
	}
}

func TestMatchRowByStrikeAndRight(t *testing.T) {
	rows := optionRowsFromChain(
		[]yfmodels.Option{{Strike: 150, Bid: 1, Ask: 2, Currency: "USD"}},
		[]yfmodels.Option{{Strike: 150, Bid: 3, Ask: 4}},
	)
	got, ok := matchRow(rows, 150, "P")
	if !ok || got.Bid != 3 {
		t.Fatalf("got %+v ok=%v, want put bid 3", got, ok)
	}
	c, ok := matchRow(rows, 150, "C")
	if !ok || c.Bid != 1 {
		t.Fatalf("got %+v ok=%v, want call bid 1", c, ok)
	}
	if _, ok := matchRow(rows, 999, "C"); ok {
		t.Fatal("expected no match for absent strike")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/plugin/ -run 'TestMarkFromRow|TestMatchRow' -v`
Expected: FAIL — `undefined: markFromRow`.

- [ ] **Step 3: Write minimal implementation**

```go
// pkg/plugin/optionchain.go
package plugin

import (
	"context"
	"fmt"
	"math"
	"time"

	yfmodels "github.com/wnjoon/go-yfinance/pkg/models"
	yfticker "github.com/wnjoon/go-yfinance/pkg/ticker"
)

// OptionRow is one matched contract side from a chain.
type OptionRow struct {
	Strike    float64
	Right     string // "C" | "P"
	Bid       float64
	Ask       float64
	LastPrice float64
	Currency  string
}

// OptionChainResult is the per-(underlying,expiry) fetch result.
type OptionChainResult struct {
	Rows               []OptionRow
	MarketState        string
	UnderlyingCurrency string
	QuoteTimeUs        int64
}

func optionRowsFromChain(calls, puts []yfmodels.Option) []OptionRow {
	out := make([]OptionRow, 0, len(calls)+len(puts))
	for _, c := range calls {
		out = append(out, OptionRow{Strike: c.Strike, Right: "C", Bid: c.Bid, Ask: c.Ask, LastPrice: c.LastPrice, Currency: c.Currency})
	}
	for _, p := range puts {
		out = append(out, OptionRow{Strike: p.Strike, Right: "P", Bid: p.Bid, Ask: p.Ask, LastPrice: p.LastPrice, Currency: p.Currency})
	}
	return out
}

// matchRow finds the contract by strike (epsilon compare) and right.
func matchRow(rows []OptionRow, strike float64, right string) (OptionRow, bool) {
	for _, r := range rows {
		if r.Right == right && math.Abs(r.Strike-strike) < 1e-6 {
			return r, true
		}
	}
	return OptionRow{}, false
}

// markFromRow returns mid when both quotes are positive, else lastPrice.
// ok is false when no usable (>0) price exists.
func markFromRow(r OptionRow) (float64, bool) {
	var mark float64
	if r.Bid > 0 && r.Ask > 0 {
		mark = (r.Bid + r.Ask) / 2
	} else {
		mark = r.LastPrice
	}
	if mark <= 0 {
		return 0, false
	}
	return mark, true
}

// FetchOptionChain pulls one (underlying, expiry) chain from Yahoo. Takes a
// limiter token like FetchBars.
func (c *YfClient) FetchOptionChain(ctx context.Context, underlyingSymbol string, expiry time.Time) (*OptionChainResult, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	t, err := yfticker.New(underlyingSymbol)
	if err != nil {
		return nil, fmt.Errorf("ticker new %s: %w", underlyingSymbol, err)
	}
	chain, err := t.OptionChainAtExpiry(expiry)
	if err != nil {
		return nil, fmt.Errorf("option chain %s %s: %w", underlyingSymbol, expiry.Format("2006-01-02"), err)
	}
	res := &OptionChainResult{Rows: optionRowsFromChain(chain.Calls, chain.Puts)}
	if chain.Underlying != nil {
		res.MarketState = chain.Underlying.MarketState
		res.UnderlyingCurrency = chain.Underlying.Currency
		res.QuoteTimeUs = chain.Underlying.RegularMarketTime * 1_000_000
	}
	return res, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/plugin/ -run 'TestMarkFromRow|TestMatchRow' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/optionchain.go pkg/plugin/optionchain_test.go
git commit -m "feat(options): chain transform, strike+right match, mid-fallback-last mark"
```

---

### Task 3: Namespace + publish helper (`publish.go`, `option_publish.go`)

**Files:**
- Modify: `pkg/plugin/publish.go` (add namespace const)
- Create: `pkg/plugin/option_publish.go`
- Test: `pkg/plugin/option_publish_test.go`

**Interfaces:**
- Consumes: `rwPGClient.Exec`, `datakey.DataKey`, `OptionMarkNamespace`.
- Produces: `func publishOptionMark(ctx context.Context, client rwPGClient, pluginID, occID, portfolioID string, mark float64, currency string, observedUs int64) error` and `const OptionMarkNamespace = "prices.option_mark"`.

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/option_publish_test.go
package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/opencapital-dev/oc-plugin-sdk/pluginclient"
)

// capClient captures the last Exec for assertions; other methods are no-ops.
type capClient struct {
	lastSQL  string
	lastArgs []any
}

func (c *capClient) Exec(_ context.Context, sql string, args ...any) (int64, error) {
	c.lastSQL, c.lastArgs = sql, args
	return 1, nil
}
func (c *capClient) Query(context.Context, string, ...any) (pluginclient.Result, error) {
	return pluginclient.Result{}, nil
}
func (c *capClient) PGExec(context.Context, string, ...any) (int64, error)  { return 0, nil }
func (c *capClient) PGQuery(context.Context, string, ...any) (pluginclient.Result, error) {
	return pluginclient.Result{}, nil
}
func (c *capClient) Config() pluginclient.Config { return pluginclient.Config{PluginID: "basic-data-app"} }

func TestPublishOptionMark(t *testing.T) {
	c := &capClient{}
	err := publishOptionMark(context.Background(), c, "basic-data-app", "AAPL 17JAN25 150 C", "pf1", 2.2, "USD", 1_700_000_000_000_000)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(c.lastSQL, "INSERT INTO data_log") {
		t.Fatalf("sql missing insert: %q", c.lastSQL)
	}
	if c.lastArgs[0] != OptionMarkNamespace {
		t.Errorf("arg0 = %v, want %v", c.lastArgs[0], OptionMarkNamespace)
	}
	if c.lastArgs[1] != "AAPL 17JAN25 150 C" || c.lastArgs[2] != "pf1" {
		t.Errorf("source_id/portfolio args wrong: %v %v", c.lastArgs[1], c.lastArgs[2])
	}
	payload, _ := c.lastArgs[7].(string)
	if !strings.Contains(payload, `"close":2.2`) || !strings.Contains(payload, `"currency":"USD"`) {
		t.Errorf("payload wrong: %s", payload)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/plugin/ -run TestPublishOptionMark -v`
Expected: FAIL — `undefined: publishOptionMark` / `undefined: OptionMarkNamespace`.

- [ ] **Step 3: Write minimal implementation**

Add to `pkg/plugin/publish.go` (alongside `OhlcvNamespace`, `QuoteNamespace`):

```go
	OptionMarkNamespace = "prices.option_mark"
```

Create `pkg/plugin/option_publish.go`:

```go
// pkg/plugin/option_publish.go
package plugin

import (
	"context"
	"encoding/json"

	"github.com/opencapital-dev/oc-plugin-sdk/datakey"
)

// publishOptionMark writes one prices.option_mark row to data_log. mark is the
// per-share option premium (contract_multiplier applies downstream). observedUs
// is the mark observation time in unix micros.
func publishOptionMark(ctx context.Context, client rwPGClient, pluginID, occID, portfolioID string, mark float64, currency string, observedUs int64) error {
	payloadJSON, err := json.Marshal(map[string]any{"close": mark, "currency": currency})
	if err != nil {
		return err
	}
	rwKey := datakey.DataKey(pluginID, OptionMarkNamespace, portfolioID, occID, observedUs)
	_, err = client.Exec(ctx, `
		INSERT INTO data_log
			(source_namespace, source_id, portfolio_id, observed_at, ingest_ts, source, plugin_id, trace_id, payload, rw_key)
		VALUES ($1, $2, $3, to_timestamp($4::double precision / 1e6), now(), $5, $6, $7, $8, $9)
	`,
		OptionMarkNamespace, occID, portfolioID, observedUs,
		"yahoo_options", pluginID, "", string(payloadJSON), rwKey,
	)
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/plugin/ -run TestPublishOptionMark -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/publish.go pkg/plugin/option_publish.go pkg/plugin/option_publish_test.go
git commit -m "feat(options): add prices.option_mark namespace + publish helper"
```

---

### Task 4: Underlying mapping read + lazy seed (`option_underlyings_repo.go`)

Reads/seeds `option_underlying` rows in `basic_data.instrument_ticker_mapping`.

**Files:**
- Create: `pkg/plugin/option_underlyings_repo.go`
- Test: `pkg/plugin/option_underlyings_repo_test.go`

**Interfaces:**
- Consumes: `rwPGClient.PGQuery`/`PGExec`, `nowMicros()`.
- Produces:
  - `type underlyingMapping struct { Symbol string; Subscribed bool }`
  - `func resolveOptionUnderlying(ctx context.Context, client rwPGClient, root, portfolioID string) (underlyingMapping, error)` — returns the existing subscribed mapping; on no row, seeds `(root, portfolioID)` with `symbol=root, subscribed=true, vendor_meta={"kind":"option_underlying"}` and returns it.

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/option_underlyings_repo_test.go
package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/opencapital-dev/oc-plugin-sdk/pluginclient"
)

// seedClient: PGQuery returns empty (no row) the first time, capturing the
// seed PGExec.
type seedClient struct {
	rows     [][]any
	seededID string
}

func (c *seedClient) Exec(context.Context, string, ...any) (int64, error) { return 0, nil }
func (c *seedClient) Query(context.Context, string, ...any) (pluginclient.Result, error) {
	return pluginclient.Result{}, nil
}
func (c *seedClient) PGExec(_ context.Context, sql string, args ...any) (int64, error) {
	if strings.Contains(sql, "INSERT INTO basic_data.instrument_ticker_mapping") {
		c.seededID, _ = args[0].(string)
	}
	return 1, nil
}
func (c *seedClient) PGQuery(context.Context, string, ...any) (pluginclient.Result, error) {
	return pluginclient.Result{Columns: []pluginclient.Column{{Name: "symbol"}, {Name: "subscribed"}}, Rows: c.rows}, nil
}
func (c *seedClient) Config() pluginclient.Config { return pluginclient.Config{} }

func TestResolveOptionUnderlyingSeedsWhenAbsent(t *testing.T) {
	c := &seedClient{rows: nil} // no existing row
	m, err := resolveOptionUnderlying(context.Background(), c, "AAPL", "pf1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m.Symbol != "AAPL" || !m.Subscribed {
		t.Fatalf("got %+v, want AAPL/subscribed", m)
	}
	if c.seededID != "AAPL" {
		t.Fatalf("did not seed, seededID=%q", c.seededID)
	}
}

func TestResolveOptionUnderlyingUsesExisting(t *testing.T) {
	c := &seedClient{rows: [][]any{{"^SPX", true}}}
	m, err := resolveOptionUnderlying(context.Background(), c, "SPX", "pf1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m.Symbol != "^SPX" || !m.Subscribed {
		t.Fatalf("got %+v, want ^SPX/subscribed", m)
	}
	if c.seededID != "" {
		t.Fatal("should not seed when row exists")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/plugin/ -run TestResolveOptionUnderlying -v`
Expected: FAIL — `undefined: resolveOptionUnderlying`.

- [ ] **Step 3: Write minimal implementation**

```go
// pkg/plugin/option_underlyings_repo.go
package plugin

import "context"

type underlyingMapping struct {
	Symbol     string
	Subscribed bool
}

// resolveOptionUnderlying returns the option_underlying mapping for
// (root, portfolioID). On first sight (no row) it seeds a default
// symbol=root, subscribed=true row and returns it.
func resolveOptionUnderlying(ctx context.Context, client rwPGClient, root, portfolioID string) (underlyingMapping, error) {
	res, err := client.PGQuery(ctx,
		`SELECT symbol, subscribed FROM basic_data.instrument_ticker_mapping
		 WHERE instrument_id = $1 AND portfolio_id = $2`, root, portfolioID)
	if err != nil {
		return underlyingMapping{}, err
	}
	if len(res.Rows) > 0 {
		col := colIndex(res.Columns)
		sub := true
		if b, ok := res.Rows[0][col["subscribed"]].(bool); ok {
			sub = b
		}
		return underlyingMapping{Symbol: rwString(res.Rows[0][col["symbol"]]), Subscribed: sub}, nil
	}
	now := nowMicros()
	if _, err := client.PGExec(ctx,
		`INSERT INTO basic_data.instrument_ticker_mapping
			(instrument_id, portfolio_id, symbol, vendor_meta, subscribed, created_at, updated_at, updated_by)
		 VALUES ($1, $2, $3, '{"kind":"option_underlying"}'::jsonb, TRUE, $4, $4, 'option-poll')
		 ON CONFLICT (instrument_id, portfolio_id) DO NOTHING`,
		root, portfolioID, root, now); err != nil {
		return underlyingMapping{}, err
	}
	return underlyingMapping{Symbol: root, Subscribed: true}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/plugin/ -run TestResolveOptionUnderlying -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/option_underlyings_repo.go pkg/plugin/option_underlyings_repo_test.go
git commit -m "feat(options): resolve+lazy-seed option_underlying mappings"
```

---

### Task 5: Poll loop (`option_poll.go`) + runtime wiring

Per-tick: load held options (RW `instruments_catalog`), parse OCC, resolve underlying, group by `(symbol, expiry)`, fetch once, gate on `MarketState`, match+mark+publish. The loop reads enable/interval from `app_settings` each cycle.

**Files:**
- Create: `pkg/plugin/option_poll.go`
- Modify: `pkg/plugin/app.go` (add `stopOptionPoll`, start in `ensureRuntime`, stop in `Dispose`)
- Test: `pkg/plugin/option_poll_test.go`

**Interfaces:**
- Consumes: `ParseOcc`, `resolveOptionUnderlying`, `matchRow`, `markFromRow`, `publishOptionMark`, `heldPairs` (existing in `discovery.go`), `rwPGClient`, `*YfClient`.
- Produces:
  - `type chainFetchFn func(ctx context.Context, symbol string, expiry time.Time) (*OptionChainResult, error)`
  - `func runOptionPollOnce(ctx context.Context, client rwPGClient, pluginID string, fetch chainFetchFn) (published int)`
  - `func StartOptionPollLoop(ctx context.Context, client rwPGClient, yf *YfClient, pluginID string) context.CancelFunc`
  - `func readOptionPollSettings(ctx context.Context, client rwPGClient) (enable bool, intervalSec int)`

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/option_poll_test.go
package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/opencapital-dev/oc-plugin-sdk/pluginclient"
)

// pollClient: Query returns one held option; PGQuery returns a subscribed
// underlying mapping; Exec captures publishes.
type pollClient struct {
	published int
}

func (c *pollClient) Exec(context.Context, string, ...any) (int64, error) {
	c.published++
	return 1, nil
}
func (c *pollClient) Query(context.Context, string, ...any) (pluginclient.Result, error) {
	return pluginclient.Result{
		Columns: []pluginclient.Column{{Name: "portfolio_id"}, {Name: "instrument_id"}, {Name: "kind"}, {Name: "currency"}, {Name: "base_currency"}, {Name: "first_seen_ts"}},
		// Expiry MUST be in the future or runOptionPollOnce's expired-guard skips it.
		// 15JAN27 is future relative to this plan's 2026-06-25 date; bump if needed at exec time.
		Rows:    [][]any{{"pf1", "AAPL 15JAN27 150 C", "option", "USD", "USD", int64(0)}},
	}, nil
}
func (c *pollClient) PGExec(context.Context, string, ...any) (int64, error) { return 0, nil }
func (c *pollClient) PGQuery(context.Context, string, ...any) (pluginclient.Result, error) {
	return pluginclient.Result{
		Columns: []pluginclient.Column{{Name: "symbol"}, {Name: "subscribed"}},
		Rows:    [][]any{{"AAPL", true}},
	}, nil
}
func (c *pollClient) Config() pluginclient.Config { return pluginclient.Config{} }

// chainStub returns a one-call-one-strike chain. Strike 150 / right C matches
// the held fixture (AAPL 15JAN27 150 C) in pollClient.Query.
func chainStub(market string) chainFetchFn {
	return func(_ context.Context, _ string, _ time.Time) (*OptionChainResult, error) {
		return &OptionChainResult{
			MarketState: market, UnderlyingCurrency: "USD", QuoteTimeUs: 1_700_000_000_000_000,
			Rows: []OptionRow{{Strike: 150, Right: "C", Bid: 2.0, Ask: 2.4}},
		}, nil
	}
}

func TestRunOptionPollOncePublishesMatchedMark(t *testing.T) {
	c := &pollClient{}
	n := runOptionPollOnce(context.Background(), c, "basic-data-app", chainStub("REGULAR"))
	if n != 1 || c.published != 1 {
		t.Fatalf("published n=%d exec=%d, want 1/1", n, c.published)
	}
}

func TestRunOptionPollOnceSkipsWhenMarketClosed(t *testing.T) {
	c := &pollClient{}
	n := runOptionPollOnce(context.Background(), c, "basic-data-app", chainStub("CLOSED"))
	if n != 0 || c.published != 0 {
		t.Fatalf("published n=%d exec=%d, want 0/0 when closed", n, c.published)
	}
}
```

> **Expired-contract caveat:** `runOptionPollOnce` skips contracts whose expiry is before today, so the held fixture MUST use a future expiry. It uses `AAPL 15JAN27 150 C` (future vs the 2026-06-25 plan date). At execution time, confirm that date is still in the future; if not, bump the fixture's expiry (in `pollClient.Query`) — `chainStub`'s `Strike: 150 / Right: C` must keep matching.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/plugin/ -run TestRunOptionPollOnce -v`
Expected: FAIL — `undefined: runOptionPollOnce`.

- [ ] **Step 3: Write minimal implementation**

```go
// pkg/plugin/option_poll.go
package plugin

import (
	"context"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

type chainFetchFn func(ctx context.Context, symbol string, expiry time.Time) (*OptionChainResult, error)

type heldOption struct {
	occ          OccParts
	occID        string
	portfolioID  string
}

type chainGroup struct {
	symbol  string
	expiry  time.Time
	members []heldOption
}

// runOptionPollOnce performs one full poll pass and returns the number of marks
// published. fetch is injected for testability.
func runOptionPollOnce(ctx context.Context, client rwPGClient, pluginID string, fetch chainFetchFn) int {
	pairs, err := heldPairs(ctx, client)
	if err != nil {
		log.DefaultLogger.Warn("option poll: heldPairs failed", "err", err)
		return 0
	}

	// Build groups keyed by (yahooSymbol, expiry); resolve underlying per root.
	groups := map[string]*chainGroup{}
	for _, p := range pairs {
		if p.Kind != "option" {
			continue
		}
		parts, perr := ParseOcc(p.InstrumentID)
		if perr != nil {
			continue
		}
		if parts.Expiry.Before(time.Now().Truncate(24 * time.Hour)) {
			continue // expired
		}
		m, merr := resolveOptionUnderlying(ctx, client, parts.Underlying, p.PortfolioID)
		if merr != nil || !m.Subscribed || m.Symbol == "" {
			continue
		}
		key := m.Symbol + "|" + parts.Expiry.Format("2006-01-02")
		g := groups[key]
		if g == nil {
			g = &chainGroup{symbol: m.Symbol, expiry: parts.Expiry}
			groups[key] = g
		}
		g.members = append(g.members, heldOption{occ: parts, occID: p.InstrumentID, portfolioID: p.PortfolioID})
	}

	published := 0
	for _, g := range groups {
		chain, ferr := fetch(ctx, g.symbol, g.expiry)
		if ferr != nil {
			log.DefaultLogger.Warn("option poll: fetch failed", "symbol", g.symbol, "err", ferr)
			continue
		}
		if chain == nil || chain.MarketState != "REGULAR" {
			continue // market-hours gate
		}
		observedUs := chain.QuoteTimeUs
		if observedUs <= 0 {
			observedUs = nowMicros()
		}
		for _, mem := range g.members {
			row, ok := matchRow(chain.Rows, mem.occ.Strike, mem.occ.Right)
			if !ok {
				continue
			}
			mark, ok := markFromRow(row)
			if !ok {
				continue
			}
			ccy := firstNonEmpty(row.Currency, chain.UnderlyingCurrency, "USD")
			if err := publishOptionMark(ctx, client, pluginID, mem.occID, mem.portfolioID, mark, ccy, observedUs); err != nil {
				log.DefaultLogger.Warn("option poll: publish failed", "occ", mem.occID, "err", err)
				continue
			}
			published++
		}
	}
	return published
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// readOptionPollSettings reads enable + interval from app_settings, defaulting
// to enabled / 900s when unset or unparseable.
func readOptionPollSettings(ctx context.Context, client rwPGClient) (bool, int) {
	enable, interval := true, 900
	res, err := client.PGQuery(ctx,
		`SELECT key, value FROM basic_data.app_settings WHERE key IN ('option_poll_enable','option_poll_interval_sec')`)
	if err != nil {
		return enable, interval
	}
	for _, row := range res.Rows {
		k := rwString(row[0])
		v := rwString(row[1])
		switch k {
		case "option_poll_enable":
			enable = v != "false"
		case "option_poll_interval_sec":
			if n := atoiDefault(v, 900); n > 0 {
				interval = n
			}
		}
	}
	return enable, interval
}

// StartOptionPollLoop runs runOptionPollOnce on the configured interval. The
// loop re-reads settings each cycle so changes take effect without restart.
func StartOptionPollLoop(ctx context.Context, client rwPGClient, yf *YfClient, pluginID string) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		fetch := func(c context.Context, symbol string, expiry time.Time) (*OptionChainResult, error) {
			return yf.FetchOptionChain(c, symbol, expiry)
		}
		for {
			enable, interval := readOptionPollSettings(ctx, client)
			if enable {
				n := runOptionPollOnce(ctx, client, pluginID, fetch)
				log.DefaultLogger.Debug("option poll tick", "published", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(interval) * time.Second):
			}
		}
	}()
	return cancel
}
```

Add a small int helper if one does not already exist (check `rw_helpers.go` first; if absent, add to `option_poll.go`):

```go
import "strconv"

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
```

Wire into `pkg/plugin/app.go`:

- Add field to the `App` struct (after `stopDiscovery`):

```go
	stopOptionPoll context.CancelFunc
```

- In `ensureRuntime`, after the `StartDiscoveryLoop` call:

```go
	a.stopOptionPoll = StartOptionPollLoop(runCtx, a.client, a.yf, a.pluginID)
```

- In `Dispose`, after the `stopBackfill` block:

```go
	if a.stopOptionPoll != nil {
		a.stopOptionPoll()
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/plugin/ -run TestRunOptionPollOnce -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/option_poll.go pkg/plugin/option_poll_test.go pkg/plugin/app.go
git commit -m "feat(options): poll loop (group/gate/match/publish) + runtime wiring"
```

---

## Phase B — Backend HTTP surface

### Task 6: Option-poll settings in `/settings`

Persist `option_poll_enable` + `option_poll_interval_sec` to `app_settings`; expose in GET/PUT.

**Files:**
- Modify: `pkg/plugin/handlers_settings.go`
- Test: `pkg/plugin/handlers_settings_test.go` (create)

**Interfaces:**
- Produces: GET `/settings` JSON gains `optionPollEnable bool`, `optionPollIntervalSec int`. PUT accepts the same two optional fields and upserts them into `app_settings`.

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/handlers_settings_test.go
package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/opencapital-dev/oc-plugin-sdk/pluginclient"
)

type settingsClient struct{ execs []string }

func (c *settingsClient) Exec(context.Context, string, ...any) (int64, error) { return 0, nil }
func (c *settingsClient) Query(context.Context, string, ...any) (pluginclient.Result, error) {
	return pluginclient.Result{}, nil
}
func (c *settingsClient) PGExec(_ context.Context, sql string, args ...any) (int64, error) {
	c.execs = append(c.execs, sql)
	return 1, nil
}
func (c *settingsClient) PGQuery(_ context.Context, sql string, args ...any) (pluginclient.Result, error) {
	// option_poll keys lookup → return enable=false, interval=600
	if strings.Contains(sql, "option_poll") {
		return pluginclient.Result{
			Columns: []pluginclient.Column{{Name: "key"}, {Name: "value"}},
			Rows:    [][]any{{"option_poll_enable", "false"}, {"option_poll_interval_sec", "600"}},
		}, nil
	}
	return pluginclient.Result{}, nil // fred key absent
}
func (c *settingsClient) Config() pluginclient.Config { return pluginclient.Config{} }

func TestSettingsGetIncludesOptionPoll(t *testing.T) {
	a := &App{client: &settingsClient{}, options: AppOptions{DiscoveryPollSec: 15, YfinanceQPS: 1, YfinanceBurst: 3}}
	body := optionPollSettings(context.Background(), a.client) // helper under test
	if body.Enable != false || body.IntervalSec != 600 {
		t.Fatalf("got %+v, want enable=false interval=600", body)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/plugin/ -run TestSettingsGetIncludesOptionPoll -v`
Expected: FAIL — `undefined: optionPollSettings`.

- [ ] **Step 3: Write minimal implementation**

In `pkg/plugin/handlers_settings.go`:

- Extend `settingsPayload`:

```go
	OptionPollEnable      *bool `json:"optionPollEnable,omitempty"`
	OptionPollIntervalSec *int  `json:"optionPollIntervalSec,omitempty"`
```

- Add a helper + use it in GET:

```go
type optionPollBody struct {
	Enable      bool
	IntervalSec int
}

func optionPollSettings(ctx context.Context, client rwPGClient) optionPollBody {
	enable, interval := readOptionPollSettings(ctx, client)
	return optionPollBody{Enable: enable, IntervalSec: interval}
}
```

- In the GET branch, add to the response map:

```go
		op := optionPollSettings(ctx, a.client)
		// ... existing writeJSON map gains:
		"optionPollEnable":      op.Enable,
		"optionPollIntervalSec": op.IntervalSec,
```

- In the PUT branch, after the FRED-key block:

```go
		if p.OptionPollEnable != nil {
			val := "true"
			if !*p.OptionPollEnable {
				val = "false"
			}
			if _, err := a.client.PGExec(ctx,
				`INSERT INTO basic_data.app_settings (key, value, updated_at) VALUES ($1, $2, now())
				 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
				"option_poll_enable", val); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if p.OptionPollIntervalSec != nil {
			if _, err := a.client.PGExec(ctx,
				`INSERT INTO basic_data.app_settings (key, value, updated_at) VALUES ($1, $2, now())
				 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
				"option_poll_interval_sec", strconv.Itoa(*p.OptionPollIntervalSec)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
```

Add imports `"context"` and `"strconv"` to the file.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/plugin/ -run TestSettingsGetIncludesOptionPoll -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/handlers_settings.go pkg/plugin/handlers_settings_test.go
git commit -m "feat(options): option-poll enable/interval in /settings"
```

---

### Task 7: Option-underlyings handler (`handlers_option_underlyings.go`) + routes

Backs the Options tab: list `option_underlying` mappings with rollup; upsert symbol / toggle subscribe.

**Files:**
- Create: `pkg/plugin/handlers_option_underlyings.go`
- Modify: `pkg/plugin/routing.go`
- Test: `pkg/plugin/handlers_option_underlyings_test.go`

**Interfaces:**
- Produces:
  - `GET /yf/option-underlyings` → `[]optionUnderlyingRow` where `type optionUnderlyingRow struct { Root string \`json:"root"\`; PortfolioID string \`json:"portfolio_id"\`; Symbol string \`json:"symbol"\`; Subscribed bool \`json:"subscribed"\`; HeldContracts int \`json:"held_contracts"\` }`
  - `POST /yf/option-underlyings` body `{ root, portfolio_id, symbol?, subscribed? }` → `{ ok: true }` (upsert).
  - `func (a *App) handleOptionUnderlyings(w http.ResponseWriter, r *http.Request)`

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/handlers_option_underlyings_test.go
package plugin

import (
	"testing"
)

func TestRollupHeldContracts(t *testing.T) {
	// pure helper: count held option contracts per (root, portfolio)
	pairs := []heldPair{
		{PortfolioID: "pf1", InstrumentID: "AAPL 17JAN25 150 C", Kind: "option"},
		{PortfolioID: "pf1", InstrumentID: "AAPL 17JAN25 160 C", Kind: "option"},
		{PortfolioID: "pf1", InstrumentID: "MSFT 17JAN25 400 P", Kind: "option"},
		{PortfolioID: "pf1", InstrumentID: "AAPL", Kind: "equity"},
	}
	got := heldContractsByRoot(pairs)
	if got["AAPL|pf1"] != 2 || got["MSFT|pf1"] != 1 {
		t.Fatalf("got %v, want AAPL|pf1=2 MSFT|pf1=1", got)
	}
	if _, ok := got["AAPL|pf1"]; !ok {
		t.Fatal("missing AAPL")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/plugin/ -run TestRollupHeldContracts -v`
Expected: FAIL — `undefined: heldContractsByRoot`.

- [ ] **Step 3: Write minimal implementation**

```go
// pkg/plugin/handlers_option_underlyings.go
package plugin

import (
	"encoding/json"
	"net/http"
)

type optionUnderlyingRow struct {
	Root          string `json:"root"`
	PortfolioID   string `json:"portfolio_id"`
	Symbol        string `json:"symbol"`
	Subscribed    bool   `json:"subscribed"`
	HeldContracts int    `json:"held_contracts"`
}

type optionUnderlyingUpsert struct {
	Root        string `json:"root"`
	PortfolioID string `json:"portfolio_id"`
	Symbol      *string `json:"symbol,omitempty"`
	Subscribed  *bool   `json:"subscribed,omitempty"`
}

// heldContractsByRoot counts held option contracts keyed "root|portfolio".
func heldContractsByRoot(pairs []heldPair) map[string]int {
	out := map[string]int{}
	for _, p := range pairs {
		if p.Kind != "option" {
			continue
		}
		parts, err := ParseOcc(p.InstrumentID)
		if err != nil {
			continue
		}
		out[parts.Underlying+"|"+p.PortfolioID]++
	}
	return out
}

func (a *App) handleOptionUnderlyings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		pairs, err := heldPairs(ctx, a.client)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		counts := heldContractsByRoot(pairs)
		rows := make([]optionUnderlyingRow, 0, len(counts))
		for key, n := range counts {
			root, pf := splitKey(key)
			m, err := resolveOptionUnderlying(ctx, a.client, root, pf)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			rows = append(rows, optionUnderlyingRow{
				Root: root, PortfolioID: pf, Symbol: m.Symbol,
				Subscribed: m.Subscribed, HeldContracts: n,
			})
		}
		writeJSON(w, rows)
	case http.MethodPost:
		var p optionUnderlyingUpsert
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if p.Root == "" || p.PortfolioID == "" {
			http.Error(w, "root and portfolio_id required", http.StatusBadRequest)
			return
		}
		// Ensure a row exists, then patch provided fields.
		if _, err := resolveOptionUnderlying(ctx, a.client, p.Root, p.PortfolioID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now := nowMicros()
		if p.Symbol != nil {
			if _, err := a.client.PGExec(ctx,
				`UPDATE basic_data.instrument_ticker_mapping SET symbol = $3, updated_at = $4, updated_by = 'options-tab'
				 WHERE instrument_id = $1 AND portfolio_id = $2`,
				p.Root, p.PortfolioID, *p.Symbol, now); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if p.Subscribed != nil {
			if _, err := a.client.PGExec(ctx,
				`UPDATE basic_data.instrument_ticker_mapping SET subscribed = $3, updated_at = $4, updated_by = 'options-tab'
				 WHERE instrument_id = $1 AND portfolio_id = $2`,
				p.Root, p.PortfolioID, *p.Subscribed, now); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
```

In `pkg/plugin/routing.go`, add inside `registerRoutes`:

```go
	mux.HandleFunc("/yf/option-underlyings", a.handleOptionUnderlyings)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/plugin/ -run TestRollupHeldContracts -v && go build ./...`
Expected: PASS + clean build.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/handlers_option_underlyings.go pkg/plugin/handlers_option_underlyings_test.go pkg/plugin/routing.go
git commit -m "feat(options): /yf/option-underlyings list+upsert handler"
```

---

### Task 8: Overview handler (`handlers_overview.go`) + route

**Files:**
- Create: `pkg/plugin/handlers_overview.go`
- Modify: `pkg/plugin/routing.go`
- Test: `pkg/plugin/handlers_overview_test.go`

**Interfaces:**
- Produces:
  - `GET /yf/overview` → `type overviewBody struct { HeldEquities int \`json:"held_equities"\`; HeldOptions int \`json:"held_options"\`; OptionUnderlyings int \`json:"option_underlyings"\`; LastOptionMarkUs int64 \`json:"last_option_mark_us"\` }`
  - `func overviewFromPairs(pairs []heldPair) (equities, options, underlyings int)`
  - `func (a *App) handleOverview(w http.ResponseWriter, r *http.Request)`

- [ ] **Step 1: Write the failing test**

```go
// pkg/plugin/handlers_overview_test.go
package plugin

import "testing"

func TestOverviewFromPairs(t *testing.T) {
	pairs := []heldPair{
		{PortfolioID: "pf1", InstrumentID: "AAPL", Kind: "equity"},
		{PortfolioID: "pf1", InstrumentID: "AAPL 17JAN25 150 C", Kind: "option"},
		{PortfolioID: "pf1", InstrumentID: "AAPL 17JAN25 160 C", Kind: "option"},
		{PortfolioID: "pf2", InstrumentID: "MSFT 17JAN25 400 P", Kind: "option"},
	}
	eq, opt, und := overviewFromPairs(pairs)
	if eq != 1 || opt != 3 || und != 2 {
		t.Fatalf("got eq=%d opt=%d und=%d, want 1/3/2", eq, opt, und)
	}
}
```

> `und` (distinct underlyings) = `{AAPL|pf1, MSFT|pf2}` = 2.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/plugin/ -run TestOverviewFromPairs -v`
Expected: FAIL — `undefined: overviewFromPairs`.

- [ ] **Step 3: Write minimal implementation**

```go
// pkg/plugin/handlers_overview.go
package plugin

import "net/http"

type overviewBody struct {
	HeldEquities     int   `json:"held_equities"`
	HeldOptions      int   `json:"held_options"`
	OptionUnderlyings int  `json:"option_underlyings"`
	LastOptionMarkUs int64 `json:"last_option_mark_us"`
}

func overviewFromPairs(pairs []heldPair) (equities, options, underlyings int) {
	roots := map[string]struct{}{}
	for _, p := range pairs {
		if p.Kind == "option" {
			options++
			if parts, err := ParseOcc(p.InstrumentID); err == nil {
				roots[parts.Underlying+"|"+p.PortfolioID] = struct{}{}
			}
			continue
		}
		equities++
	}
	return equities, options, len(roots)
}

func (a *App) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	pairs, err := heldPairs(ctx, a.client)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	eq, opt, und := overviewFromPairs(pairs)
	body := overviewBody{HeldEquities: eq, HeldOptions: opt, OptionUnderlyings: und}

	res, err := a.client.Query(ctx,
		`SELECT max(observed_at) FROM data_log WHERE source_namespace = $1`, OptionMarkNamespace)
	if err == nil && len(res.Rows) > 0 {
		body.LastOptionMarkUs = rwMicros(res.Rows[0][0])
	}
	writeJSON(w, body)
}
```

In `pkg/plugin/routing.go`, add inside `registerRoutes`:

```go
	mux.HandleFunc("/yf/overview", a.handleOverview)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/plugin/ -run TestOverviewFromPairs -v && go test ./pkg/plugin/ -v && go build ./...`
Expected: PASS (whole package) + clean build.

- [ ] **Step 5: Commit**

```bash
git add pkg/plugin/handlers_overview.go pkg/plugin/handlers_overview_test.go pkg/plugin/routing.go
git commit -m "feat(options): /yf/overview status rollup handler"
```

---

## Phase C — Frontend (nav, shell, pages)

> **Before each frontend task, invoke `/frontend-design:frontend-design`** to design layout, spacing, hierarchy, and states deliberately. The shell + sections below are the minimum; the skill guides visual polish (the current Settings page has no margins/sections — see spec screenshot).

### Task 9: Padded/sectioned page shell + section primitive

Fix the no-margin look. `Page` currently wraps `PluginPage` with no padding; `SettingsPage` doesn't even use `Page`.

**Files:**
- Modify: `src/components/Page.tsx`
- Create: `src/components/Section.tsx`
- Test: `src/components/Section.test.tsx`

**Interfaces:**
- Produces:
  - `Page.Contents` now renders a padded, max-width container with vertical rhythm.
  - `Section` component: `function Section(props: { title: string; description?: string; children: React.ReactNode }): JSX.Element` — a titled card/fieldset block.

- [ ] **Step 1: Write the failing test**

```tsx
// src/components/Section.test.tsx
import React from 'react';
import { render, screen } from '@testing-library/react';
import { Section } from './Section';

test('renders title, description, and children', () => {
  render(
    <Section title="Option polling" description="Controls the poll loop">
      <div>child-content</div>
    </Section>,
  );
  expect(screen.getByText('Option polling')).toBeInTheDocument();
  expect(screen.getByText('Controls the poll loop')).toBeInTheDocument();
  expect(screen.getByText('child-content')).toBeInTheDocument();
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `npm run test -- --watchAll=false src/components/Section.test.tsx`
Expected: FAIL — cannot find module `./Section`.

- [ ] **Step 3: Write minimal implementation**

```tsx
// src/components/Section.tsx
import React, { type ReactNode } from 'react';
import { css } from '@emotion/css';
import { type GrafanaTheme2 } from '@grafana/data';
import { useStyles2 } from '@grafana/ui';

export function Section({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  const s = useStyles2(getStyles);
  return (
    <section className={s.card}>
      <header className={s.header}>
        <h3 className={s.title}>{title}</h3>
        {description && <p className={s.desc}>{description}</p>}
      </header>
      <div className={s.body}>{children}</div>
    </section>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  card: css({
    background: theme.colors.background.secondary,
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
    padding: theme.spacing(3),
    marginBottom: theme.spacing(3),
  }),
  header: css({ marginBottom: theme.spacing(2) }),
  title: css({ margin: 0, fontSize: theme.typography.h4.fontSize }),
  desc: css({ margin: theme.spacing(0.5, 0, 0), color: theme.colors.text.secondary, fontSize: theme.typography.bodySmall.fontSize }),
  body: css({ display: 'flex', flexDirection: 'column', gap: theme.spacing(2) }),
});
```

Update `src/components/Page.tsx` `Contents` to add a padded, max-width container:

```tsx
import { css } from '@emotion/css';
import { type GrafanaTheme2 } from '@grafana/data';
import { useStyles2 } from '@grafana/ui';

// ...existing Page wrapper unchanged...

function Contents({ children }: { children?: ReactNode }) {
  const s = useStyles2(getContentStyles);
  return <div className={s.wrap}>{children}</div>;
}

const getContentStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    padding: theme.spacing(3),
    maxWidth: 960,
    margin: '0 auto',
    width: '100%',
    display: 'flex',
    flexDirection: 'column',
    gap: theme.spacing(2),
  }),
});
```

- [ ] **Step 4: Run test to verify it passes**

Run: `npm run test -- --watchAll=false src/components/Section.test.tsx`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/components/Section.tsx src/components/Section.test.tsx src/components/Page.tsx
git commit -m "feat(ui): padded/sectioned page shell + Section primitive"
```

---

### Task 10: Nav restructure + Overview page + API clients

**Files:**
- Modify: `src/constants.ts`, `src/plugin.json`, `src/components/App/App.tsx`
- Create: `src/pages/OverviewPage.tsx`, `src/api/overview.ts`, `src/api/options.ts`
- Test: `src/pages/OverviewPage.test.tsx`

**Interfaces:**
- Consumes: `yfRequest` from `src/api/client.ts`, `Page`, `Section`.
- Produces:
  - `ROUTES` enum gains `Overview = 'overview'`, renames `Tickers` → `Instruments = 'instruments'`.
  - `getOverview(): Promise<Overview>` where `Overview = { held_equities: number; held_options: number; option_underlyings: number; last_option_mark_us: number }`.
  - `listOptionUnderlyings()`, `setOptionUnderlyingSymbol(root, portfolio_id, symbol)`, `toggleOptionUnderlying(root, portfolio_id, subscribed)` in `api/options.ts`.

- [ ] **Step 1: Write the failing test**

```tsx
// src/pages/OverviewPage.test.tsx
import React from 'react';
import { render, screen, waitFor } from '@testing-library/react';
import { OverviewPage } from './OverviewPage';

jest.mock('../api/overview', () => ({
  getOverview: jest.fn().mockResolvedValue({
    held_equities: 5, held_options: 3, option_underlyings: 2, last_option_mark_us: 0,
  }),
}));

test('renders held counts', async () => {
  render(<OverviewPage />);
  await waitFor(() => expect(screen.getByText('5')).toBeInTheDocument());
  expect(screen.getByText('3')).toBeInTheDocument();
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `npm run test -- --watchAll=false src/pages/OverviewPage.test.tsx`
Expected: FAIL — cannot find `./OverviewPage`.

- [ ] **Step 3: Write minimal implementation**

`src/api/overview.ts`:

```ts
import { yfRequest } from './client';

export type Overview = {
  held_equities: number;
  held_options: number;
  option_underlyings: number;
  last_option_mark_us: number;
};

export const getOverview = () => yfRequest<Overview>('/overview');
```

`src/api/options.ts`:

```ts
import { yfRequest } from './client';

export type OptionUnderlying = {
  root: string;
  portfolio_id: string;
  symbol: string;
  subscribed: boolean;
  held_contracts: number;
};

export const listOptionUnderlyings = () => yfRequest<OptionUnderlying[]>('/option-underlyings');

export const setOptionUnderlyingSymbol = (root: string, portfolio_id: string, symbol: string) =>
  yfRequest<{ ok: boolean }>('/option-underlyings', { method: 'POST', body: { root, portfolio_id, symbol } });

export const toggleOptionUnderlying = (root: string, portfolio_id: string, subscribed: boolean) =>
  yfRequest<{ ok: boolean }>('/option-underlyings', { method: 'POST', body: { root, portfolio_id, subscribed } });
```

`src/constants.ts` — replace the `ROUTES` enum:

```ts
export enum ROUTES {
  Overview = 'overview',
  Instruments = 'instruments',
  Settings = 'settings',
}
```

`src/pages/OverviewPage.tsx`:

```tsx
import React, { useEffect, useState } from 'react';
import { Stack } from '@grafana/ui';

import { Page } from '../components/Page';
import { Section } from '../components/Section';
import { getOverview, type Overview } from '../api/overview';

function Stat({ label, value }: { label: string; value: number | string }) {
  return (
    <Stack direction="column" gap={0.5}>
      <span style={{ fontSize: 28, fontWeight: 600 }}>{value}</span>
      <span style={{ opacity: 0.7 }}>{label}</span>
    </Stack>
  );
}

export function OverviewPage() {
  const [o, setO] = useState<Overview | null>(null);
  useEffect(() => {
    getOverview().then(setO);
  }, []);
  const lastMark = o && o.last_option_mark_us > 0 ? new Date(o.last_option_mark_us / 1000).toLocaleString() : '—';
  return (
    <Page>
      <Page.Contents>
        <h2>Overview</h2>
        <Section title="Holdings" description="Instruments discovered from imported portfolios">
          <Stack direction="row" gap={4}>
            <Stat label="Equities" value={o?.held_equities ?? '—'} />
            <Stat label="Options" value={o?.held_options ?? '—'} />
            <Stat label="Option underlyings" value={o?.option_underlyings ?? '—'} />
          </Stack>
        </Section>
        <Section title="Option marks" description="Latest Yahoo option-mark poll">
          <Stat label="Last option mark" value={lastMark} />
        </Section>
      </Page.Contents>
    </Page>
  );
}
```

`src/plugin.json` — replace the `includes` array:

```json
  "includes": [
    { "type": "page", "name": "Overview", "path": "/a/%PLUGIN_ID%/overview", "addToNav": true, "defaultNav": true, "icon": "home-alt" },
    { "type": "page", "name": "Instruments", "path": "/a/%PLUGIN_ID%/instruments", "addToNav": true, "icon": "chart-line" },
    { "type": "page", "name": "Settings", "path": "/a/%PLUGIN_ID%/settings", "addToNav": true, "icon": "cog" }
  ],
```

`src/components/App/App.tsx` — update routes (note: `InstrumentsPage` arrives in Task 11; until then, import `TickersPage` and alias the route. To keep this task building, point Instruments at `TickersPage` temporarily):

```tsx
import React from 'react';
import { Navigate, Route, Routes } from 'react-router-dom';
import { type AppRootProps } from '@grafana/data';

import { ROUTES } from '../../constants';
import { OverviewPage } from '../../pages/OverviewPage';
import { TickersPage } from '../../pages/TickersPage';
import { SettingsPage } from '../../pages/SettingsPage';

function App(_props: AppRootProps) {
  return (
    <Routes>
      <Route path={ROUTES.Overview} element={<OverviewPage />} />
      <Route path={ROUTES.Instruments} element={<TickersPage />} />
      <Route path={ROUTES.Settings} element={<SettingsPage />} />
      <Route path="*" element={<Navigate replace to={ROUTES.Overview} />} />
    </Routes>
  );
}

export default App;
```

> Update `src/components/App/App.test.tsx`: the `initialEntries` that used `/tickers` should use `/instruments`, and add an `/overview` default-redirect assertion mirroring the existing pattern.

- [ ] **Step 4: Run tests to verify they pass**

Run: `npm run test -- --watchAll=false src/pages/OverviewPage.test.tsx src/components/App`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/constants.ts src/plugin.json src/components/App/App.tsx src/components/App/App.test.tsx src/pages/OverviewPage.tsx src/pages/OverviewPage.test.tsx src/api/overview.ts src/api/options.ts
git commit -m "feat(ui): Overview landing + nav restructure + options/overview api"
```

---

### Task 11: Instruments tabs (Stocks/Options) + Settings sections

Split the instruments page into tabs and rework Settings into sections with the option-poll controls.

**Files:**
- Create: `src/pages/InstrumentsPage.tsx`, `src/components/instruments/StocksTab.tsx`, `src/components/instruments/OptionsTab.tsx`, `src/components/instruments/OptionsTab.test.tsx`
- Modify: `src/pages/TickersPage.tsx` → becomes `StocksTab` body (see below), `src/components/App/App.tsx` (point Instruments at `InstrumentsPage`), `src/pages/SettingsPage.tsx`, `src/api/settings.ts`
- Test: `src/pages/SettingsPage.test.tsx` (create)

**Interfaces:**
- Consumes: `listOptionUnderlyings`, `setOptionUnderlyingSymbol`, `toggleOptionUnderlying` (Task 10); `Section`, `Page`; existing `getSettings`/`putSettings`.
- Produces: `Settings` type gains `optionPollEnable: boolean`, `optionPollIntervalSec: number`. `InstrumentsPage` renders a `TabsBar` with `Stocks` + `Options`.

- [ ] **Step 1: Write the failing test**

```tsx
// src/components/instruments/OptionsTab.test.tsx
import React from 'react';
import { render, screen, waitFor } from '@testing-library/react';
import { OptionsTab } from './OptionsTab';

jest.mock('../../api/options', () => ({
  listOptionUnderlyings: jest.fn().mockResolvedValue([
    { root: 'AAPL', portfolio_id: 'pf1', symbol: 'AAPL', subscribed: true, held_contracts: 2 },
  ]),
  setOptionUnderlyingSymbol: jest.fn(),
  toggleOptionUnderlying: jest.fn(),
}));

test('lists option underlyings with held count', async () => {
  render(<OptionsTab />);
  await waitFor(() => expect(screen.getByText('AAPL')).toBeInTheDocument());
  expect(screen.getByText('2')).toBeInTheDocument();
});
```

```tsx
// src/pages/SettingsPage.test.tsx
import React from 'react';
import { render, screen, waitFor } from '@testing-library/react';
import { SettingsPage } from './SettingsPage';

jest.mock('../api/settings', () => ({
  getSettings: jest.fn().mockResolvedValue({
    fred_api_key_set: true, pollIntervalSec: 15, qps: 1, burst: 3,
    liveEnable: true, backfillEnable: true,
    optionPollEnable: true, optionPollIntervalSec: 900,
  }),
  putSettings: jest.fn().mockResolvedValue({ ok: true }),
  testFred: jest.fn(),
}));

test('renders Option polling section with interval', async () => {
  render(<SettingsPage />);
  await waitFor(() => expect(screen.getByText('Option polling')).toBeInTheDocument());
  expect(screen.getByDisplayValue('900')).toBeInTheDocument();
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `npm run test -- --watchAll=false src/components/instruments/OptionsTab.test.tsx src/pages/SettingsPage.test.tsx`
Expected: FAIL — modules not found / fields missing.

- [ ] **Step 3: Write minimal implementation**

`src/api/settings.ts` — extend the `Settings` type:

```ts
export type Settings = {
  fred_api_key_set: boolean;
  pollIntervalSec: number;
  qps: number;
  burst: number;
  liveEnable: boolean;
  backfillEnable: boolean;
  optionPollEnable: boolean;
  optionPollIntervalSec: number;
};
```

`src/components/instruments/StocksTab.tsx` — **mechanical extraction**: move the entire body of `src/pages/TickersPage.tsx` into a `StocksTab` component, unchanged, EXCEPT:
- rename `export function TickersPage()` → `export function StocksTab()`;
- remove the outer `<Page>...</Page>` wrapper (return only the inner content — the `InstrumentsPage` provides `Page`);
- keep all imports/logic/styles identical.
Then delete `src/pages/TickersPage.tsx`.

`src/components/instruments/OptionsTab.tsx`:

```tsx
import React, { useEffect, useState } from 'react';
import { Button, Input, Switch, Stack } from '@grafana/ui';

import {
  listOptionUnderlyings,
  setOptionUnderlyingSymbol,
  toggleOptionUnderlying,
  type OptionUnderlying,
} from '../../api/options';

export function OptionsTab() {
  const [rows, setRows] = useState<OptionUnderlying[]>([]);
  const [edits, setEdits] = useState<Record<string, string>>({});

  const load = () => listOptionUnderlyings().then(setRows);
  useEffect(() => {
    load();
  }, []);

  const key = (r: OptionUnderlying) => `${r.root}|${r.portfolio_id}`;

  return (
    <table style={{ width: '100%' }}>
      <thead>
        <tr>
          <th style={{ textAlign: 'left' }}>Underlying</th>
          <th style={{ textAlign: 'left' }}>Yahoo symbol</th>
          <th style={{ textAlign: 'left' }}>Held contracts</th>
          <th style={{ textAlign: 'left' }}>Subscribed</th>
          <th />
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <tr key={key(r)}>
            <td>{r.root}</td>
            <td>
              <Input
                value={edits[key(r)] ?? r.symbol}
                onChange={(e) => setEdits({ ...edits, [key(r)]: e.currentTarget.value })}
                width={20}
              />
            </td>
            <td>{r.held_contracts}</td>
            <td>
              <Switch
                value={r.subscribed}
                onChange={async () => {
                  await toggleOptionUnderlying(r.root, r.portfolio_id, !r.subscribed);
                  load();
                }}
              />
            </td>
            <td>
              <Button
                size="sm"
                variant="secondary"
                onClick={async () => {
                  await setOptionUnderlyingSymbol(r.root, r.portfolio_id, edits[key(r)] ?? r.symbol);
                  load();
                }}
              >
                Save
              </Button>
            </td>
          </tr>
        ))}
        {rows.length === 0 && (
          <tr>
            <td colSpan={5} style={{ opacity: 0.7, padding: 12 }}>
              No option positions discovered yet.
            </td>
          </tr>
        )}
      </tbody>
    </table>
  );
}
```

`src/pages/InstrumentsPage.tsx`:

```tsx
import React, { useState } from 'react';
import { TabsBar, Tab, TabContent } from '@grafana/ui';

import { Page } from '../components/Page';
import { StocksTab } from '../components/instruments/StocksTab';
import { OptionsTab } from '../components/instruments/OptionsTab';

export function InstrumentsPage() {
  const [tab, setTab] = useState<'stocks' | 'options'>('stocks');
  return (
    <Page>
      <Page.Contents>
        <h2>Instruments</h2>
        <TabsBar>
          <Tab label="Stocks" active={tab === 'stocks'} onChangeTab={() => setTab('stocks')} />
          <Tab label="Options" active={tab === 'options'} onChangeTab={() => setTab('options')} />
        </TabsBar>
        <TabContent>
          {tab === 'stocks' ? <StocksTab /> : <OptionsTab />}
        </TabContent>
      </Page.Contents>
    </Page>
  );
}
```

`src/components/App/App.tsx` — swap the Instruments route target:

```tsx
import { InstrumentsPage } from '../../pages/InstrumentsPage';
// remove the TickersPage import
// ...
      <Route path={ROUTES.Instruments} element={<InstrumentsPage />} />
```

`src/pages/SettingsPage.tsx` — rework into sections using `Page` + `Section`, keeping FRED behavior and adding option-poll controls:

```tsx
import React, { useEffect, useState } from 'react';
import { Alert, Button, Field, Input, SecretInput, Switch } from '@grafana/ui';

import { Page } from '../components/Page';
import { Section } from '../components/Section';
import { getSettings, putSettings, testFred, type Settings } from '../api/settings';

export function SettingsPage() {
  const [s, setS] = useState<Settings | null>(null);
  const [key, setKey] = useState('');
  const [msg, setMsg] = useState<string | null>(null);

  useEffect(() => {
    getSettings().then(setS);
  }, []);

  const saveFred = async () => {
    await putSettings({ fred_api_key: key });
    setMsg('Saved');
    setKey('');
    getSettings().then(setS);
  };

  const savePoll = async (patch: Partial<Settings>) => {
    await putSettings(patch);
    setMsg('Saved');
    getSettings().then(setS);
  };

  const test = async () => {
    const r = await testFred();
    setMsg(r.ok ? 'FRED key OK' : 'FRED key invalid');
  };

  return (
    <Page>
      <Page.Contents>
        <h2>Settings</h2>

        <Section title="API keys">
          <Field label="FRED API key" description="Get one at fredaccount.stlouisfed.org. Stored locally.">
            <SecretInput
              isConfigured={!!s?.fred_api_key_set}
              value={key}
              placeholder={s?.fred_api_key_set ? 'configured' : 'enter key'}
              onChange={(e) => setKey(e.currentTarget.value)}
              onReset={() => setKey('')}
            />
          </Field>
          <Button onClick={saveFred}>Save</Button>{' '}
          <Button variant="secondary" onClick={test}>
            Test
          </Button>
        </Section>

        <Section title="Option polling" description="Yahoo option-chain marks for held option positions.">
          <Field label="Enabled" description="Poll held option chains and publish marks.">
            <Switch
              value={s?.optionPollEnable ?? true}
              onChange={() => savePoll({ optionPollEnable: !(s?.optionPollEnable ?? true) })}
            />
          </Field>
          <Field label="Interval (seconds)" description="How often to poll (default 900 = 15 min).">
            <Input
              type="number"
              width={20}
              value={s?.optionPollIntervalSec ?? 900}
              onChange={(e) => setS(s ? { ...s, optionPollIntervalSec: Number(e.currentTarget.value) } : s)}
              onBlur={() => s && savePoll({ optionPollIntervalSec: s.optionPollIntervalSec })}
            />
          </Field>
        </Section>

        {msg && <Alert title={msg} severity="info" />}
      </Page.Contents>
    </Page>
  );
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `npm run test -- --watchAll=false src/components/instruments/OptionsTab.test.tsx src/pages/SettingsPage.test.tsx && npm run typecheck`
Expected: PASS + typecheck clean.

- [ ] **Step 5: Commit**

```bash
git add src/pages/InstrumentsPage.tsx src/components/instruments/ src/pages/SettingsPage.tsx src/pages/SettingsPage.test.tsx src/api/settings.ts src/components/App/App.tsx
git rm src/pages/TickersPage.tsx
git commit -m "feat(ui): Instruments Stocks/Options tabs + sectioned Settings with option polling"
```

---

### Task 12: Full verification + e2e smoke

**Files:** none (verification only).

- [ ] **Step 1: Backend test + build**

Run: `go test ./... && go vet ./... && go build ./...`
Expected: all PASS, clean build.

- [ ] **Step 2: Frontend test + typecheck + lint + build**

Run: `npm run test -- --watchAll=false && npm run typecheck && npm run lint && npm run build`
Expected: all PASS.

- [ ] **Step 3: Manual smoke (document result)**

Build the plugin (`mage -v` for backend, `npm run build` for frontend), load in the app, and verify:
- Plugin lands on **Overview** (not Instruments).
- **Instruments** shows **Stocks** + **Options** tabs; Stocks behaves as before.
- **Settings** shows **API keys** + **Option polling** sections with proper margins/spacing (the original no-margin issue is gone).
- With a held option position, after one poll interval a `prices.option_mark` row with `source='yahoo_options'` appears in `data_log` and the option's MtM updates.

- [ ] **Step 4: Commit (if any verification fixups were needed)**

```bash
git add -A
git commit -m "chore(options): verification fixups"
```

---

## Self-Review notes (for the executor)

- **Spec coverage:** OCC parse (T1), chain/mark (T2), namespace/publish (T3), underlying map seed (T4), poll loop+gate+grouping+settings-driven cadence (T5), settings fields (T6), Options-tab backend (T7), Overview backend (T8), page shell/sections fixing the no-margin bug (T9), Overview page + nav demotion + default→Overview (T10), Stocks/Options tabs + sectioned Settings + option-poll controls (T11), verification (T12). Statement+Yahoo coexistence and accumulate-never-delete need no code (ASOF + not extending the DELETE).
- **Frontend-design:** invoke `/frontend-design:frontend-design` at the start of T9–T11 for visual polish beyond the minimum shell.
- **Type consistency:** Go `OptionChainResult`/`OptionRow`/`OccParts`/`underlyingMapping`/`heldPair` used consistently; TS `Settings`, `Overview`, `OptionUnderlying`, `ROUTES` consistent across tasks.
- **Known v1 limits (from spec):** no historical backfill; index/non-US underlyings rely on the Options-tab override (`SPX → ^SPX`); Overview is read-only.
