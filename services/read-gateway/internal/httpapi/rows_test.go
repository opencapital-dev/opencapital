package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/portfolio-management/read-gateway/internal/rw"
)

// capturingReader records the SQL it was asked to run so a test can assert that
// two requests compile to the same query (used to prove mode reconstruction).
type capturingReader struct {
	rows rw.Rows
	sql  string
}

func (c *capturingReader) Query(_ context.Context, sql string, _ ...any) (rw.Rows, error) {
	c.sql = sql
	return c.rows, nil
}

func doRows(t *testing.T, s *Server, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/rows", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	return rr
}

// TestRows_ScopedRows proves /v1/rows returns the org-scoped rows for a selector
// in the wire shape {columns, rows}, with cells encoded as-is (ts int, NULL null).
func TestRows_ScopedRows(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	s.Reader = fakeReader{rows: rw.Rows{
		Columns: []string{"ts", "nav_base"},
		Rows:    [][]any{{int64(1000), 100.5}, {int64(2000), nil}},
	}}

	rr := doRows(t, s, "tok",
		`{"selector":"portfolio{portfolio=\"`+pid.String()+`\"}","mode":"latest","from":0,"to":2000}`)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var got RowsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if want := []string{"ts", "nav_base"}; !equalStrs(got.Columns, want) {
		t.Fatalf("columns: got %v want %v", got.Columns, want)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows: got %d want 2", len(got.Rows))
	}
	if !strings.Contains(rr.Body.String(), `[1000,100.5]`) {
		t.Errorf("ts/number encoding wrong: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `[2000,null]`) {
		t.Errorf("null cell encoding wrong: %s", rr.Body.String())
	}
}

// TestRows_EmptyRowsIsArray proves an empty result serialises rows as [] (never
// JSON null) so the polars client can build a typed zero-height frame.
func TestRows_EmptyRowsIsArray(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	s.Reader = fakeReader{rows: rw.Rows{Columns: []string{"ts", "nav_base"}, Rows: nil}}

	rr := doRows(t, s, "tok",
		`{"selector":"portfolio{portfolio=\"`+pid.String()+`\"}","mode":"latest","from":0,"to":2000}`)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"rows":[]`) {
		t.Errorf("empty rows must be [], got: %s", rr.Body.String())
	}
}

// TestRows_ForeignPortfolio403 proves /v1/rows scopes out a foreign-org
// portfolio with the SAME 403 the compute/raw paths use — no new tenancy logic.
func TestRows_ForeignPortfolio403(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	other := uuid.New()
	rr := doRows(t, s, "tok",
		`{"selector":"instrument{portfolio=\"`+other.String()+`\"}","mode":"latest","from":0,"to":2000}`)
	if rr.Code != 403 {
		t.Fatalf("want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRows_BadBearer401(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	s := newTestServer(org, pid)
	if rr := doRows(t, s, "", `{}`); rr.Code != 401 {
		t.Fatalf("missing bearer want 401, got %d", rr.Code)
	}
	if rr := doRows(t, s, "bad", `{"selector":"portfolio{}","mode":null,"from":0,"to":1}`); rr.Code != 401 {
		t.Fatalf("bad token want 401, got %d", rr.Code)
	}
}

// TestRows_ModeReconstruction proves the {selector, mode} split reproduces the
// inline-@mode selector: a request with mode:"latest" compiles to the SAME SQL
// as fetchRows would for "<selector> @latest", and mode:null uses the bare
// selector (DSL default mode = asof), yielding different SQL.
func TestRows_ModeReconstruction(t *testing.T) {
	org, pid := uuid.New(), uuid.New()
	bare := `portfolio{portfolio="` + pid.String() + `"}`

	newSrv := func() (*Server, *capturingReader) {
		cr := &capturingReader{rows: rw.Rows{Columns: []string{"ts"}, Rows: [][]any{{int64(1)}}}}
		s := newTestServer(org, pid)
		s.Reader = cr
		return s, cr
	}

	// Endpoint with mode:"latest".
	s, cr := newSrv()
	rr := doRows(t, s, "tok",
		`{"selector":`+jsonStr(bare)+`,"mode":"latest","from":0,"to":2000}`)
	if rr.Code != 200 {
		t.Fatalf("latest want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	gotLatest := cr.sql

	// fetchRows with the inline-@latest selector — the string the client stripped.
	s2, cr2 := newSrv()
	if _, st, err := s2.fetchRows(context.Background(), org, "", "tok", bare+" @latest", 0, 2000); err != nil {
		t.Fatalf("fetchRows @latest: status=%d err=%v", st, err)
	}
	if gotLatest != cr2.sql {
		t.Fatalf("mode:\"latest\" SQL mismatch:\n endpoint: %s\n fetchRows: %s", gotLatest, cr2.sql)
	}

	// Endpoint with mode:null uses the bare selector (default asof) → different SQL.
	s3, cr3 := newSrv()
	rr3 := doRows(t, s3, "tok",
		`{"selector":`+jsonStr(bare)+`,"mode":null,"from":0,"to":2000}`)
	if rr3.Code != 200 {
		t.Fatalf("null mode want 200, got %d: %s", rr3.Code, rr3.Body.String())
	}
	if cr3.sql == gotLatest {
		t.Fatalf("mode:null should compile differently from @latest, both: %s", cr3.sql)
	}

	// mode:null matches the bare (no-@mode) selector.
	s4, cr4 := newSrv()
	if _, st, err := s4.fetchRows(context.Background(), org, "", "tok", bare, 0, 2000); err != nil {
		t.Fatalf("fetchRows bare: status=%d err=%v", st, err)
	}
	if cr3.sql != cr4.sql {
		t.Fatalf("mode:null SQL mismatch vs bare selector:\n endpoint: %s\n fetchRows: %s", cr3.sql, cr4.sql)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
