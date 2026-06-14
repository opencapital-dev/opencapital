package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/portfolio-management/control-plane/internal/config"
)

func TestAddSourceRejectsEmpty(t *testing.T) {
	s := &Server{cfg: config.Config{}, logger: slog.Default()}
	rr := httptest.NewRecorder()
	s.handleAddSource(rr, httptest.NewRequest(http.MethodPost, "/v1/sources", strings.NewReader(`{}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rr.Code)
	}
}

func TestDeleteSourceRequiresURL(t *testing.T) {
	s := &Server{cfg: config.Config{}, logger: slog.Default()}
	rr := httptest.NewRecorder()
	s.handleDeleteSource(rr, httptest.NewRequest(http.MethodDelete, "/v1/sources", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rr.Code)
	}
}
