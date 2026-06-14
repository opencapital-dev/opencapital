package compile

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/portfolio-management/read-gateway/internal/viewschema"
	"github.com/opencapital-dev/oc-plugin-sdk/dsl"
)

// stubSchema returns canned columns per view, satisfying the compiler's schema
// interface so tests need no live RisingWave.
type stubSchema map[string][]viewschema.Column

func (s stubSchema) Columns(view string) ([]viewschema.Column, error) {
	cols, ok := s[view]
	if !ok {
		return nil, errUnknownView(view)
	}
	return cols, nil
}

type errUnknownView string

func (e errUnknownView) Error() string { return "unknown view " + string(e) }

func str(name string) viewschema.Column { return viewschema.Column{Name: name, Numeric: false} }
func num(name string) viewschema.Column { return viewschema.Column{Name: name, Numeric: true} }

// navCols models e_nav as the normalized view exposes it: org + portfolio
// scoped (scope column is `portfolio`, NO `portfolio_id`), a value metric, a ts
// axis.
func navCols() []viewschema.Column {
	return []viewschema.Column{str("org_id"), str("portfolio"), num("value"), num("ts")}
}

// priceCols models e_price: a portfolio + instrument scoped core view.
func priceCols() []viewschema.Column {
	return []viewschema.Column{str("org_id"), str("portfolio"), str("instrument"), num("ts"), num("value"), str("currency")}
}

// catalogCols models a discovery view (instruments_catalog): it exposes BOTH
// the friendly and the legacy id columns.
func catalogCols() []viewschema.Column {
	return []viewschema.Column{
		str("org_id"), str("portfolio"), str("portfolio_id"),
		str("instrument"), str("instrument_id"), num("ts"), str("kind"),
	}
}

func mustParseQ(t *testing.T, q string) dsl.Selector {
	t.Helper()
	s, err := dsl.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	return s
}

func TestCompiler_OrgInjectedEveryMode(t *testing.T) {
	// nav resolves to e_nav; assert org_id = $1 on asof, window, latest.
	sc := stubSchema{
		"e_nav":               navCols(),
		"instruments_catalog": catalogCols(),
	}
	c := &Compiler{schema: sc}
	org := uuid.New()

	for _, q := range []string{`nav{} @asof`, `nav{} @window`} {
		sql, args, err := c.Compile(mustParseQ(t, q), org, nil, "", 1000, 2000)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if !strings.Contains(sql, "org_id = $1") {
			t.Errorf("%s: missing org_id = $1:\n%s", q, sql)
		}
		if args[0] != org {
			t.Errorf("%s: args[0] != org: %v", q, args[0])
		}
	}

	// @latest on a grain-bearing entity.
	sql, args, err := c.Compile(mustParseQ(t, `instruments_used{} @latest`), org, nil, "", 1000, 2000)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if !strings.Contains(sql, "org_id = $1") {
		t.Errorf("latest: missing org_id = $1:\n%s", sql)
	}
	if args[0] != org {
		t.Errorf("latest: args[0] != org: %v", args[0])
	}
}

func TestCompiler_RefusesViewWithoutOrgID(t *testing.T) {
	// A view whose canned columns LACK org_id must be refused with no SQL.
	sc := stubSchema{"e_nav": {str("portfolio"), num("value"), num("ts")}}
	c := &Compiler{schema: sc}
	sql, _, err := c.Compile(mustParseQ(t, `nav{} @asof`), uuid.New(), nil, "", 1000, 2000)
	if err == nil {
		t.Fatal("expected error for view lacking org_id, got nil")
	}
	if sql != "" {
		t.Errorf("expected empty SQL on refusal, got: %s", sql)
	}
}

func TestCompiler_UnknownProjectionRejected(t *testing.T) {
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	_, _, err := c.Compile(mustParseQ(t, `nav{}[nav_base, not_a_col] @asof`), uuid.New(), nil, "", 1000, 2000)
	if err == nil {
		t.Fatal("expected error for unknown projection column")
	}
}

func TestCompiler_UnknownStringMatcherRejected(t *testing.T) {
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	_, _, err := c.Compile(mustParseQ(t, `nav{not_a_col="x"} @asof`), uuid.New(), nil, "", 1000, 2000)
	if err == nil {
		t.Fatal("expected error for unknown string matcher column")
	}
}

func TestCompiler_NumericMatcherOnNonNumericRejected(t *testing.T) {
	// portfolio is non-numeric; a numeric matcher on it must be rejected.
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	_, _, err := c.Compile(mustParseQ(t, `nav{portfolio > 5} @asof`), uuid.New(), nil, "", 1000, 2000)
	if err == nil {
		t.Fatal("expected error for numeric matcher on non-numeric column")
	}
}

func TestCompiler_PortfolioScopeConditional(t *testing.T) {
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	org := uuid.New()
	port := uuid.New()

	// Injected when portfolio non-nil and view has portfolio column.
	sql, args, err := c.Compile(mustParseQ(t, `nav{} @asof`), org, &port, "", 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "portfolio = $2") {
		t.Errorf("portfolio scope not injected:\n%s", sql)
	}
	if args[1] != port {
		t.Errorf("args[1] != port: %v", args[1])
	}

	// Not injected when portfolio nil.
	sql2, _, err := c.Compile(mustParseQ(t, `nav{} @asof`), org, nil, "", 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql2, "portfolio =") {
		t.Errorf("portfolio scope should not be injected when nil:\n%s", sql2)
	}
}

func TestCompiler_InstrumentColumnIsNotScoped(t *testing.T) {
	// e_price is portfolio + instrument scoped, but only portfolio is a tenancy
	// scope: instrument is data. A non-nil portfolio scopes on `portfolio` and
	// never on `instrument`.
	sc := stubSchema{"e_price": priceCols()}
	c := &Compiler{schema: sc}
	org := uuid.New()
	port := uuid.New()
	sql, _, err := c.Compile(mustParseQ(t, `price{} @window`), org, &port, "", 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "portfolio = $2") {
		t.Errorf("portfolio scope not injected:\n%s", sql)
	}
	if strings.Contains(sql, "instrument = $") {
		t.Errorf("instrument must not be injected as a scope predicate:\n%s", sql)
	}
}

func TestCompiler_PortfolioScopeInjectedForCoreView(t *testing.T) {
	// Regression guard for Defect 1: the core e_nav view exposes scope as
	// `portfolio` (not `portfolio_id`); a non-nil portfolio MUST scope the query.
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	org := uuid.New()
	port := uuid.New()
	sql, args, err := c.Compile(mustParseQ(t, `nav{} @asof`), org, &port, "", 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "portfolio = $2") {
		t.Fatalf("portfolio scope not injected on core view:\n%s", sql)
	}
	if args[1] != port {
		t.Errorf("args[1] != port: %v", args[1])
	}
}

func TestCompiler_PluginScope(t *testing.T) {
	sc := stubSchema{"ohlcv_coverage": {str("org_id"), str("source_id"), str("plugin_id"), num("ts")}}
	c := &Compiler{schema: sc}
	org := uuid.New()

	// Injected when view has plugin_id and pluginID non-empty.
	sql, args, err := c.Compile(mustParseQ(t, `ohlcv_coverage{} @latest`), org, nil, "yfinance", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "plugin_id = $") {
		t.Errorf("plugin scope not injected:\n%s", sql)
	}
	found := false
	for _, a := range args {
		if a == "yfinance" {
			found = true
		}
	}
	if !found {
		t.Errorf("plugin id not bound: %v", args)
	}

	// Error when plugin-scoped but pluginID empty.
	if _, _, err := c.Compile(mustParseQ(t, `ohlcv_coverage{} @latest`), org, nil, "", 0, 2000); err == nil {
		t.Fatal("expected error for plugin-scoped view with empty pluginID")
	}
}

func TestCompiler_TimePredicateArgsAreInt64Micros(t *testing.T) {
	// Regression guard for the dropped microsToTS: ts bounds bind the raw int64
	// microseconds, NOT a time.Time.
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	const fromUS int64 = 1_000_000_000
	const toUS int64 = 2_000_000_000

	sql, args, err := c.Compile(mustParseQ(t, `nav{} @window`), uuid.New(), nil, "", fromUS, toUS)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "ts >= $") || !strings.Contains(sql, "ts <= $") {
		t.Fatalf("window missing both ts bounds:\n%s", sql)
	}
	var gotFrom, gotTo bool
	for _, a := range args {
		if v, ok := a.(int64); ok {
			if v == fromUS {
				gotFrom = true
			}
			if v == toUS {
				gotTo = true
			}
		}
	}
	if !gotFrom || !gotTo {
		t.Fatalf("expected int64 micros %d and %d in args, got %#v", fromUS, toUS, args)
	}
}

func TestCompiler_LatestDistinctOnGrain(t *testing.T) {
	sc := stubSchema{"instruments_catalog": catalogCols()}
	c := &Compiler{schema: sc}
	sql, _, err := c.Compile(mustParseQ(t, `instruments_used{} @latest`), uuid.New(), nil, "", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "DISTINCT ON (portfolio, instrument)") {
		t.Errorf("missing distinct-on grain:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY portfolio, instrument, ts DESC") {
		t.Errorf("missing grain order-by:\n%s", sql)
	}
}

func TestCompiler_LatestDistinctOnCoreView(t *testing.T) {
	// Regression guard for Defect 2: @latest on a CORE view (e_nav, grain
	// [portfolio]) must dedupe on the friendly `portfolio` column.
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	sql, _, err := c.Compile(mustParseQ(t, `nav{} @latest`), uuid.New(), nil, "", 0, 2000)
	if err != nil {
		t.Fatalf("@latest on core view: %v", err)
	}
	if !strings.Contains(sql, "DISTINCT ON (portfolio)") {
		t.Errorf("missing distinct-on portfolio grain:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY portfolio, ts DESC") {
		t.Errorf("missing grain order-by:\n%s", sql)
	}
}

func TestCompiler_LatestEmptyGrainErrors(t *testing.T) {
	// events has grain [] in the catalog (no @latest support).
	sc := stubSchema{"e_events": {str("org_id"), str("portfolio"), str("instrument"), num("ts"), str("event_type")}}
	c := &Compiler{schema: sc}
	if _, _, err := c.Compile(mustParseQ(t, `events{} @latest`), uuid.New(), nil, "", 0, 2000); err == nil {
		t.Fatal("expected error for @latest on entity with empty grain")
	}
}

func TestCompiler_RedundantPortfolioMatcherSkipped(t *testing.T) {
	// A portfolio="x" matcher PLUS injected portfolio scope must yield a single
	// portfolio predicate (the matcher is skipped to avoid duplication).
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	org := uuid.New()
	port := uuid.New()
	sql, _, err := c.Compile(mustParseQ(t, `nav{portfolio="`+port.String()+`"} @asof`), org, &port, "", 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(sql, "portfolio ="); n != 1 {
		t.Errorf("expected exactly 1 portfolio predicate, got %d:\n%s", n, sql)
	}
}

func TestCompiler_LatestWithNumericWrapsDedupedSet(t *testing.T) {
	sc := stubSchema{
		"instruments_catalog": append(catalogCols(), num("quantity")),
	}
	c := &Compiler{schema: sc}
	sql, _, err := c.Compile(mustParseQ(t, `instruments_used{quantity != 0} @latest`), uuid.New(), nil, "", 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, ") latest WHERE") {
		t.Errorf("numeric matcher should wrap the deduped set:\n%s", sql)
	}
	if !strings.Contains(sql, "quantity != $") {
		t.Errorf("numeric predicate missing in wrapper:\n%s", sql)
	}
}

func TestCompiler_UnknownEntityRejected(t *testing.T) {
	sc := stubSchema{}
	c := &Compiler{schema: sc}
	if _, _, err := c.Compile(mustParseQ(t, `bobby_tables{} @asof`), uuid.New(), nil, "", 0, 2000); err == nil {
		t.Fatal("expected error for non-allow-listed entity")
	}
}

func TestCompiler_DefaultProjectionAllColumns(t *testing.T) {
	sc := stubSchema{"e_nav": navCols()}
	c := &Compiler{schema: sc}
	sql, _, err := c.Compile(mustParseQ(t, `nav{} @asof`), uuid.New(), nil, "", 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "SELECT org_id, portfolio, value, ts FROM e_nav") {
		t.Errorf("default projection should be all columns in order:\n%s", sql)
	}
}
