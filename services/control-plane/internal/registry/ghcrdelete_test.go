package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGHCRDeleter_DeleteVersion(t *testing.T) {
	hit := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			hit = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	d := &GHCRDeleter{apiBase: srv.URL, token: "x", httpc: srv.Client()}
	if err := d.DeletePackageVersion(context.Background(), "plugins-staging/core-datasource", "123"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hit, "/versions/123") {
		t.Fatalf("expected DELETE on /versions/123, got %q", hit)
	}

	// 404-as-success (already gone)
	srvGone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srvGone.Close)
	d2 := &GHCRDeleter{apiBase: srvGone.URL, token: "x", httpc: srvGone.Client()}
	if err := d2.DeletePackageVersion(context.Background(), "pkg", "99"); err != nil {
		t.Fatalf("404 on delete should be success, got %v", err)
	}
}

func TestGHCRDeleter_ResolveVersionID(t *testing.T) {
	versions := []map[string]any{
		{"id": 999, "metadata": map[string]any{"container": map[string]any{"tags": []string{"v9.9.9"}}}},
		{"id": 123, "metadata": map[string]any{"container": map[string]any{"tags": []string{"v1.0.0"}}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode(versions)
	}))
	t.Cleanup(srv.Close)
	d := &GHCRDeleter{apiBase: srv.URL, token: "x", httpc: srv.Client()}
	id, ok, err := d.ResolveVersionID(context.Background(), "plugins-staging/core-datasource", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || id != "123" {
		t.Fatalf("got id=%q ok=%v, want 123/true", id, ok)
	}
	// unknown tag -> ok=false, no error
	_, ok2, err := d.ResolveVersionID(context.Background(), "plugins-staging/core-datasource", "v0.0.0")
	if err != nil || ok2 {
		t.Fatalf("unknown tag: got ok=%v err=%v, want false/nil", ok2, err)
	}
}

func TestGHCRDeleter_ResolveVersionID_PackageNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	d := &GHCRDeleter{apiBase: srv.URL, token: "x", httpc: srv.Client()}
	_, ok, err := d.ResolveVersionID(context.Background(), "plugins-staging/none", "v1.0.0")
	if err != nil || ok {
		t.Fatalf("package-not-found: got ok=%v err=%v, want false/nil", ok, err)
	}
}
