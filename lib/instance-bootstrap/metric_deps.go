package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// metricVarRe matches a Grafana dashboard variable reference ($name or ${name})
// as written inside a metric .py (e.g. in an @bind selector).
var metricVarRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// injectMetricVarDeps enriches a library-panel model so Grafana re-runs the
// panel when the dashboard variables its metric uses change. The resolved
// Python lives server-side (the datasource reads it by ref), so those variables
// are invisible to Grafana's client-side variable-dependency scan, which only
// reads the stored query. For every target with a `ref`, this reads the
// referenced metric, extracts the `$var` references it already declares (in its
// @bind selectors), and stores them as a space-joined `deps` string on the
// target — a string field the scan picks up. Returns modelBytes unchanged when
// there is nothing to add (no ref, unreadable metric, or no variables).
func injectMetricVarDeps(modelBytes []byte, pluginsDir string) []byte {
	var m map[string]any
	if err := json.Unmarshal(modelBytes, &m); err != nil {
		return modelBytes
	}
	targets, ok := m["targets"].([]any)
	if !ok {
		return modelBytes
	}
	changed := false
	for _, t := range targets {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		ref, _ := tm["ref"].(string)
		if ref == "" {
			continue
		}
		if deps := metricRefVarDeps(pluginsDir, ref); deps != "" {
			tm["deps"] = deps
			changed = true
		}
	}
	if !changed {
		return modelBytes
	}
	out, err := json.Marshal(m)
	if err != nil {
		return modelBytes
	}
	return out
}

// metricRefVarDeps resolves the metric .py for ref "pluginID/metric" and returns
// its distinct `$var` references as a space-joined string (e.g. "$portfolio_id"),
// or "" when the file is unreadable or declares no variables.
func metricRefVarDeps(pluginsDir, ref string) string {
	pluginID, metric, ok := strings.Cut(ref, "/")
	if !ok || pluginID == "" || metric == "" {
		return ""
	}
	body, err := os.ReadFile(filepath.Join(pluginsDir, pluginID, "library-panels", filepath.FromSlash(metric)+".py"))
	if err != nil {
		return ""
	}
	seen := map[string]bool{}
	var deps []string
	for _, mm := range metricVarRe.FindAllStringSubmatch(string(body), -1) {
		if name := mm[1]; !seen[name] {
			seen[name] = true
			deps = append(deps, "$"+name)
		}
	}
	return strings.Join(deps, " ")
}
