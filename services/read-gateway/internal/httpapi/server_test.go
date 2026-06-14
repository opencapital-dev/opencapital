package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/portfolio-management/read-gateway/internal/compile"
	"github.com/portfolio-management/read-gateway/internal/rw"
	"github.com/portfolio-management/read-gateway/internal/viewschema"
)

type fakeVerifier struct {
	org    uuid.UUID
	plugin string
}

func (f fakeVerifier) OrgFromBearer(_ context.Context, bearer string) (uuid.UUID, error) {
	if bearer == "bad" {
		return uuid.Nil, fmt.Errorf("bad token")
	}
	return f.org, nil
}

func (f fakeVerifier) Identify(_ context.Context, bearer string) (uuid.UUID, string, error) {
	if bearer == "bad" {
		return uuid.Nil, "", fmt.Errorf("bad token")
	}
	return f.org, f.plugin, nil
}

type fakeOwn struct {
	owned map[uuid.UUID]bool
	names map[uuid.UUID]string
}

func (f fakeOwn) Owns(_ context.Context, _ uuid.UUID, _ string, p uuid.UUID) (bool, error) {
	return f.owned[p], nil
}
func (f fakeOwn) Portfolios(_ context.Context, _ uuid.UUID, _ string) (map[uuid.UUID]string, error) {
	return f.names, nil
}

type fakeReader struct{ rows rw.Rows }

func (f fakeReader) Query(_ context.Context, _ string, _ ...any) (rw.Rows, error) { return f.rows, nil }

// stubSchema is a canned viewschema.Cache stand-in used in tests.
type stubSchema map[string][]viewschema.Column

func (s stubSchema) Columns(view string) ([]viewschema.Column, error) {
	cols, ok := s[view]
	if !ok {
		return nil, fmt.Errorf("stubSchema: unknown view %q", view)
	}
	return cols, nil
}

func str(name string) viewschema.Column { return viewschema.Column{Name: name, Numeric: false} }
func num(name string) viewschema.Column { return viewschema.Column{Name: name, Numeric: true} }

// testSchema covers the views exercised by the server tests.
var testSchema = stubSchema{
	"e_instrument": {
		str("org_id"), str("portfolio"), str("instrument"), num("ts"),
		num("quantity"), str("currency"), str("direction"),
	},
	"e_portfolio": {
		str("org_id"), str("portfolio"), num("ts"),
		num("nav_base"), str("base_currency"),
	},
	"ohlcv_coverage": {
		str("org_id"), str("source_id"), str("plugin_id"), num("ts"),
	},
}

func newTestServer(org, pid uuid.UUID) *Server {
	sc := testSchema
	return &Server{
		Verifier:  fakeVerifier{org: org},
		Ownership: fakeOwn{owned: map[uuid.UUID]bool{pid: true}, names: map[uuid.UUID]string{pid: "X"}},
		Reader:    fakeReader{rows: rw.Rows{Columns: []string{"instrument_id", "quantity"}, Rows: [][]any{{"AAPL", 10.0}}}},
		Schema:    sc,
		Compiler:  compile.NewCompiler(sc),
	}
}

func do(t *testing.T, s *Server, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	return rr
}

func TestQuery_RawPassthrough(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	rr := do(t, s, "tok", `{"to":2000,"outputMode":"table","bindings":[{"name":"P","type":"","selector":"instrument{portfolio=\"`+pid.String()+`\", quantity != 0} @latest"}]}`)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "AAPL") {
		t.Fatalf("missing row: %s", rr.Body.String())
	}
}

// TestQuery_RawColumnTypesFromSchema proves the raw path types each output
// column from the introspected view schema: numeric columns map to "number",
// others to "string".
func TestQuery_RawColumnTypesFromSchema(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	sc := stubSchema{
		"e_portfolio": {
			str("org_id"), str("portfolio"), num("ts"),
			num("nav_base"), str("base_currency"),
		},
	}
	s := &Server{
		Verifier:  fakeVerifier{org: org},
		Ownership: fakeOwn{owned: map[uuid.UUID]bool{pid: true}, names: map[uuid.UUID]string{pid: "X"}},
		Reader: fakeReader{rows: rw.Rows{
			Columns: []string{"ts", "nav_base", "portfolio", "base_currency"},
			Rows:    [][]any{{int64(1000), 123.45, pid.String(), "USD"}},
		}},
		Schema:   sc,
		Compiler: compile.NewCompiler(sc),
	}
	rr := do(t, s, "tok", `{"to":2000,"outputMode":"table","bindings":[{"name":"P","type":"","selector":"portfolio{portfolio=\"`+pid.String()+`\"} @latest"}]}`)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var res Result
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	want := map[string]string{
		"ts":           "number",
		"nav_base":     "number",
		"portfolio":    "string",
		"base_currency": "string",
	}
	if len(res.Columns) != len(want) {
		t.Fatalf("column count: got %d want %d (%+v)", len(res.Columns), len(want), res.Columns)
	}
	wantOrder := []string{"ts", "nav_base", "portfolio", "base_currency"}
	for i, c := range res.Columns {
		if c.Name != wantOrder[i] {
			t.Fatalf("column %d: got name %q want %q", i, c.Name, wantOrder[i])
		}
		if c.Type != want[c.Name] {
			t.Errorf("column %q: got type %q want %q", c.Name, c.Type, want[c.Name])
		}
	}
}

func TestQuery_ForeignPortfolio403(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	other := uuid.New()
	rr := do(t, s, "tok", `{"to":2000,"bindings":[{"name":"P","type":"","selector":"instrument{portfolio=\"`+other.String()+`\"} @latest"}]}`)
	if rr.Code != 403 {
		t.Fatalf("want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestQuery_BadBearer401(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	if rr := do(t, s, "", `{}`); rr.Code != 401 {
		t.Fatalf("missing bearer want 401, got %d", rr.Code)
	}
	if rr := do(t, s, "bad", `{"bindings":[{"selector":"portfolios{}"}]}`); rr.Code != 401 {
		t.Fatalf("bad token want 401, got %d", rr.Code)
	}
}

// TestQuery_PortfoliosControlPlane proves portfolios{} is served from the
// control-plane carve-out, not the compiler path.
func TestQuery_PortfoliosControlPlane(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	rr := do(t, s, "tok", `{"bindings":[{"name":"P","type":"","selector":"portfolios{}"}]}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), pid.String()) || !strings.Contains(rr.Body.String(), `"X"`) {
		t.Fatalf("portfolios want 200+id+name, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestQuery_ScopeLabelNonEq400(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	rr := do(t, s, "tok", `{"bindings":[{"selector":"instrument{portfolio=~\"`+pid.String()+`\"} @latest"}]}`)
	if rr.Code != 400 {
		t.Fatalf("want 400 for non-eq scope op, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestQuery_PluginScopedRejectsNoPluginID(t *testing.T) {
	org := uuid.New()
	sc := testSchema
	s := &Server{
		Verifier:  fakeVerifier{org: org, plugin: ""},
		Ownership: fakeOwn{owned: map[uuid.UUID]bool{}, names: map[uuid.UUID]string{}},
		Reader:    fakeReader{rows: rw.Rows{Columns: []string{"source_id", "observed_at"}, Rows: [][]any{{"AAPL", nil}}}},
		Schema:    sc,
		Compiler:  compile.NewCompiler(sc),
	}
	rr := do(t, s, "tok", `{"to":2000,"outputMode":"table","bindings":[{"name":"A","type":"","selector":"ohlcv_coverage{} @latest"}]}`)
	if rr.Code == 200 {
		t.Fatalf("plugin-scoped query without plugin_id must not 200; got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestQuery_PluginScopedWithPluginID(t *testing.T) {
	org := uuid.New()
	sc := testSchema
	s := &Server{
		Verifier:  fakeVerifier{org: org, plugin: "yfinance-app"},
		Ownership: fakeOwn{owned: map[uuid.UUID]bool{}, names: map[uuid.UUID]string{}},
		Reader:    fakeReader{rows: rw.Rows{Columns: []string{"source_id", "observed_at"}, Rows: [][]any{{"AAPL", nil}}}},
		Schema:    sc,
		Compiler:  compile.NewCompiler(sc),
	}
	rr := do(t, s, "tok", `{"to":2000,"outputMode":"table","bindings":[{"name":"A","type":"","selector":"ohlcv_coverage{} @latest"}]}`)
	if rr.Code != 200 {
		t.Fatalf("plugin-scoped query with plugin_id should 200; got %d: %s", rr.Code, rr.Body.String())
	}
}
