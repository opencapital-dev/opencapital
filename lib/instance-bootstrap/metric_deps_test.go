package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectMetricVarDeps(t *testing.T) {
	root := t.TempDir()
	py := filepath.Join(root, "yfinance-app", "library-panels", "sector_pnl.py")
	if err := os.MkdirAll(filepath.Dir(py), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `positions="instrument{portfolio=\"$portfolio_id\"}"` + "\n" + `title="${base_currency}"`
	if err := os.WriteFile(py, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	model := []byte(`{"title":"t","targets":[{"refId":"A","ref":"yfinance-app/sector_pnl"}]}`)

	out := injectMetricVarDeps(model, root)

	var m struct {
		Targets []map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	deps, _ := m.Targets[0]["deps"].(string)
	if !strings.Contains(deps, "$portfolio_id") {
		t.Errorf("deps = %q, want it to contain $portfolio_id", deps)
	}
	if !strings.Contains(deps, "$base_currency") {
		t.Errorf("deps = %q, want it to contain $base_currency", deps)
	}
}

func TestInjectMetricVarDepsLeavesNonRefAndMissing(t *testing.T) {
	// A target with no ref is left untouched.
	model := []byte(`{"targets":[{"refId":"A","source":"x = 1"}]}`)
	if got := injectMetricVarDeps(model, t.TempDir()); string(got) != string(model) {
		t.Errorf("non-ref target changed: %s", got)
	}
	// A ref to a missing metric adds nothing.
	model2 := []byte(`{"targets":[{"refId":"A","ref":"p/missing"}]}`)
	if got := injectMetricVarDeps(model2, t.TempDir()); string(got) != string(model2) {
		t.Errorf("missing metric changed model: %s", got)
	}
}
