// Package surface defines the explicit allow-list of DSL entity names reachable
// through read-gateway and their backing RisingWave view names.
package surface

// views maps bare DSL entity name -> backing RisingWave view name.
// Sourced from catalog/*.yaml (route: rw_view entities only).
// "portfolios" (route: control_plane) and "positions" (route: rw_template) are excluded.
var views = map[string]string{
	"nav":              "e_nav",
	"flows":            "e_flows",
	"cash":             "e_cash",
	"closures":         "e_closures",
	"cycles":           "e_cycles",
	"events":           "e_events",
	"instrument":       "e_instrument",
	"portfolio":        "e_portfolio",
	"price":            "e_price",
	"data_coverage":    "data_coverage",
	"fx_pairs_used":    "fx_pairs_used",
	"instruments_used": "instruments_catalog",
	"ohlcv":            "prices_ohlcv",
	"ohlcv_coverage":   "ohlcv_coverage",
}

// grains maps a DSL entity name -> its @latest DISTINCT ON key columns. Only
// entities that support @latest appear here; everything else has an empty grain
// (events declares grain [] in the catalog) and @latest on them is rejected by
// the compiler. Columns are the friendly names the normalized views expose:
// portfolio (aliased from scope_id/portfolio_id), instrument (from
// instrument_id), cadence (the friendly bar_cadence alias on prices_ohlcv).
var grains = map[string][]string{
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

// Resolve returns the backing RisingWave view name for a bare DSL entity name.
// Names not on the allow-list return ok=false; no SQL is built for them.
func Resolve(name string) (view string, ok bool) {
	view, ok = views[name]
	return
}

// Grain returns the @latest DISTINCT ON key columns for a DSL entity name, or
// an empty slice for entities that do not support @latest. The returned slice
// must not be mutated.
func Grain(name string) []string {
	return grains[name]
}

// Names returns the backing RisingWave view names for all allow-listed entities.
// Used at boot to pre-load the viewschema cache.
func Names() []string {
	out := make([]string, 0, len(views))
	for _, v := range views {
		out = append(out, v)
	}
	return out
}
