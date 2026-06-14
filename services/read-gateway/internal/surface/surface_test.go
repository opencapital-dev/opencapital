package surface

import (
	"reflect"
	"testing"
)

func TestResolve_AllowedEntities(t *testing.T) {
	cases := []struct {
		name string
		view string
	}{
		{"nav", "e_nav"},
		{"flows", "e_flows"},
		{"cash", "e_cash"},
		{"closures", "e_closures"},
		{"cycles", "e_cycles"},
		{"events", "e_events"},
		{"instrument", "e_instrument"},
		{"portfolio", "e_portfolio"},
		{"price", "e_price"},
		{"data_coverage", "data_coverage"},
		{"fx_pairs_used", "fx_pairs_used"},
		{"instruments_used", "instruments_catalog"},
		{"ohlcv", "prices_ohlcv"},
		{"ohlcv_coverage", "ohlcv_coverage"},
	}
	for _, tc := range cases {
		view, ok := Resolve(tc.name)
		if !ok {
			t.Errorf("Resolve(%q): want ok=true, got false", tc.name)
			continue
		}
		if view != tc.view {
			t.Errorf("Resolve(%q): want view=%q, got %q", tc.name, tc.view, view)
		}
	}
}

func TestResolve_DisallowedNames(t *testing.T) {
	for _, name := range []string{
		"bobby_tables",
		"e_nav",
		"positions",
		"portfolios",
		"",
		"instruments_catalog",
	} {
		if _, ok := Resolve(name); ok {
			t.Errorf("Resolve(%q): want ok=false, got true", name)
		}
	}
}

func TestGrain_LatestEntities(t *testing.T) {
	// Grain columns are the friendly names exposed by the normalized views:
	// portfolio (not portfolio_id), instrument (not instrument_id), cadence.
	cases := map[string][]string{
		"nav":              {"portfolio"},
		"flows":            {"portfolio"},
		"cash":             {"portfolio", "currency"},
		"closures":         {"portfolio", "instrument"},
		"cycles":           {"portfolio", "instrument"},
		"instrument":       {"portfolio", "instrument"},
		"portfolio":        {"portfolio"},
		"price":            {"portfolio", "instrument"},
		"instruments_used": {"portfolio", "instrument"},
		"ohlcv":            {"portfolio", "instrument", "cadence"},
		"fx_pairs_used":    {"base_ccy", "quote_ccy"},
		"ohlcv_coverage":   {"source_id"},
		"data_coverage":    {"source_id"},
	}
	for name, want := range cases {
		if got := Grain(name); !reflect.DeepEqual(got, want) {
			t.Errorf("Grain(%q): want %v, got %v", name, want, got)
		}
	}
}

func TestGrain_NonLatestEntitiesEmpty(t *testing.T) {
	// events has grain [] in the catalog (no @latest); unknown is not on the
	// allow-list at all.
	for _, name := range []string{"events", "unknown"} {
		if got := Grain(name); len(got) != 0 {
			t.Errorf("Grain(%q): want empty, got %v", name, got)
		}
	}
}
