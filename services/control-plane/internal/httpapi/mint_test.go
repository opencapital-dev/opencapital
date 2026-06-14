package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// These tests guard the v8 /jwt/mint request contract that makes the SAME
// control-plane serve both multi-org (cloud) and single-org (local): org_id is
// supplied EXPLICITLY in the request body and is required. The control plane no
// longer derives org_id from the Grafana JWT's `aud=org:N` claim (every Grafana
// process is single-org). Single-org local is therefore just N=1 data behind
// this same contract — no mode flag, no fork. If a future refactor reintroduced
// org-from-aud (or made org_id optional), local breaks; these tests fail first.
//
// They cover only the pre-store validation branches (which return before any DB
// call), so they need no store/keys. The membership happy path (HasUserOrg at
// N=1) is exercised at integration level against a live control_db, not faked
// here — control-plane's store has no querier seam.

func mintPost(t *testing.T, body string) (int, string) {
	t.Helper()
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := httptest.NewRequest(http.MethodPost, "/jwt/mint", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleMint(rr, r)
	return rr.Code, rr.Body.String()
}

func TestMint_RequiresOrgIDInBody(t *testing.T) {
	// plugin_id present, org_id absent → 400. This is the load-bearing guard:
	// org_id must come from the body, so single-org local supplies its one org.
	code, body := mintPost(t, `{"plugin_id":"yfinance","user_id":"u"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body=%s)", code, body)
	}
	if !strings.Contains(body, "org_id required") {
		t.Fatalf("body = %q, want 'org_id required'", body)
	}
}

func TestMint_RejectsNonUUIDOrgID(t *testing.T) {
	code, body := mintPost(t, `{"plugin_id":"yfinance","org_id":"not-a-uuid","user_id":"u"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body=%s)", code, body)
	}
	if !strings.Contains(body, "org_id not a UUID") {
		t.Fatalf("body = %q, want 'org_id not a UUID'", body)
	}
}

func TestMint_RequiresPluginID(t *testing.T) {
	code, body := mintPost(t, `{"org_id":"`+uuid.NewString()+`","user_id":"u"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body=%s)", code, body)
	}
	if !strings.Contains(body, "plugin_id required") {
		t.Fatalf("body = %q, want 'plugin_id required'", body)
	}
}

func TestMint_RequiresAnAuthPath(t *testing.T) {
	// Valid org_id + plugin_id but neither platform_token (v8) nor user_id
	// (static-IdP) → 400. Guards that mint always authenticates the caller.
	code, body := mintPost(t, `{"plugin_id":"yfinance","org_id":"`+uuid.NewString()+`"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body=%s)", code, body)
	}
	if !strings.Contains(body, "platform_token or user_id required") {
		t.Fatalf("body = %q, want 'platform_token or user_id required'", body)
	}
}
