package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGHCREnumerator_PrefixFilter(t *testing.T) {
	pkgs := []map[string]any{
		{"name": "plugins-staging/core-datasource"},
		{"name": "plugins-staging/core-app"},
		{"name": "plugins/core-datasource"},
		{"name": "some-unrelated-image"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/packages" || r.URL.Query().Get("package_type") != "container" {
			http.NotFound(w, r)
			return
		}
		// page 1 returns the list; any other page returns empty (stops pagination)
		if p := r.URL.Query().Get("page"); p != "" && p != "1" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pkgs)
	}))
	t.Cleanup(srv.Close)

	en := &GHCREnumerator{apiBase: srv.URL, token: "x", httpc: srv.Client()}
	got, err := en.ReposWithPrefix(context.Background(), "plugins-staging/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"core-app", "core-datasource"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}
