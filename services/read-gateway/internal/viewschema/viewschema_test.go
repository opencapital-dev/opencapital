package viewschema

import (
	"context"
	"testing"

	"github.com/portfolio-management/read-gateway/internal/rw"
)

// stubQuerier returns canned rows for a specific view name.
type stubQuerier struct {
	// calls counts how many times Query was called.
	calls int
	// responses maps view name -> information_schema rows [[col_name, data_type], ...]
	responses map[string][][]any
}

func (s *stubQuerier) Query(_ context.Context, _ string, args ...any) (rw.Rows, error) {
	s.calls++
	view, _ := args[0].(string)
	raw := s.responses[view]
	rows := make([][]any, len(raw))
	for i, r := range raw {
		rows[i] = r
	}
	return rw.Rows{
		Columns: []string{"column_name", "data_type"},
		Rows:    rows,
	}, nil
}

func TestColumns_KnownView(t *testing.T) {
	q := &stubQuerier{responses: map[string][][]any{
		"e_nav": {
			{"org_id", "character varying"},
			{"portfolio", "character varying"},
			{"ts", "bigint"},
			{"value", "double precision"},
			{"currency", "character varying"},
		},
	}}
	c := New(q)
	if err := c.Load(context.Background(), "e_nav"); err != nil {
		t.Fatalf("Load: %v", err)
	}

	cols, err := c.Columns("e_nav")
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}

	want := []Column{
		{Name: "org_id", Numeric: false},
		{Name: "portfolio", Numeric: false},
		{Name: "ts", Numeric: true},
		{Name: "value", Numeric: true},
		{Name: "currency", Numeric: false},
	}
	if len(cols) != len(want) {
		t.Fatalf("column count: got %d want %d", len(cols), len(want))
	}
	for i, w := range want {
		if cols[i] != w {
			t.Errorf("col[%d]: got %+v want %+v", i, cols[i], w)
		}
	}
}

func TestColumns_NumericTypeMapping(t *testing.T) {
	types := []struct {
		dtype   string
		numeric bool
	}{
		{"smallint", true},
		{"integer", true},
		{"bigint", true},
		{"numeric", true},
		{"decimal", true},
		{"real", true},
		{"double precision", true},
		{"int2", true},
		{"int4", true},
		{"int8", true},
		{"float4", true},
		{"float8", true},
		{"character varying", false},
		{"text", false},
		{"boolean", false},
		{"timestamp with time zone", false},
	}

	rows := make([][]any, len(types))
	for i, tt := range types {
		rows[i] = []any{tt.dtype + "_col", tt.dtype}
	}

	q := &stubQuerier{responses: map[string][][]any{"v": rows}}
	c := New(q)
	if err := c.Load(context.Background(), "v"); err != nil {
		t.Fatal(err)
	}
	cols, err := c.Columns("v")
	if err != nil {
		t.Fatal(err)
	}
	for i, tt := range types {
		if cols[i].Numeric != tt.numeric {
			t.Errorf("type %q: numeric got %v want %v", tt.dtype, cols[i].Numeric, tt.numeric)
		}
	}
}

func TestColumns_UnknownView(t *testing.T) {
	q := &stubQuerier{responses: map[string][][]any{}}
	c := New(q)
	// Load "e_nav" which has no rows → treated as unknown on Columns call.
	if err := c.Load(context.Background(), "e_nav"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Columns("e_nav"); err == nil {
		t.Fatal("want error for view with no rows (non-existent)")
	}
	// Never loaded at all.
	if _, err := c.Columns("no_such_view"); err == nil {
		t.Fatal("want error for never-loaded view")
	}
}

func TestColumns_CacheHit_NoSecondQuery(t *testing.T) {
	q := &stubQuerier{responses: map[string][][]any{
		"e_nav": {{"ts", "bigint"}, {"value", "double precision"}},
	}}
	c := New(q)
	if err := c.Load(context.Background(), "e_nav"); err != nil {
		t.Fatal(err)
	}
	if q.calls != 1 {
		t.Fatalf("Load should call Query once; got %d", q.calls)
	}

	// Second and third Columns calls must NOT hit the querier.
	if _, err := c.Columns("e_nav"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Columns("e_nav"); err != nil {
		t.Fatal(err)
	}
	if q.calls != 1 {
		t.Fatalf("Columns must not re-query; call count got %d want 1", q.calls)
	}
}

func TestRefresh_ReloadsExistingViews(t *testing.T) {
	q := &stubQuerier{responses: map[string][][]any{
		"e_nav": {{"ts", "bigint"}},
	}}
	c := New(q)
	if err := c.Load(context.Background(), "e_nav"); err != nil {
		t.Fatal(err)
	}
	before := q.calls

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q.calls <= before {
		t.Fatalf("Refresh must re-query; calls before=%d after=%d", before, q.calls)
	}
}
