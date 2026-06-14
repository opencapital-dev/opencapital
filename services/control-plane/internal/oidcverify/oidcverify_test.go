package oidcverify

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ---------------------------------------------------------------------------
// Claim-mapping tests (pure — no network)
// ---------------------------------------------------------------------------

func TestGitHubIdentity(t *testing.T) {
	claims := map[string]any{
		"iss":              "https://token.actions.githubusercontent.com",
		"job_workflow_ref": "acme/plugin/.github/workflows/release.yml@refs/tags/v1.0.0",
	}
	got := identityFromClaims("https://token.actions.githubusercontent.com", claims)
	want := "https://token.actions.githubusercontent.com|acme/plugin/.github/workflows/release.yml@refs/tags/v1.0.0"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDexEmailIdentity(t *testing.T) {
	claims := map[string]any{
		"iss":   "http://dex:5556/dex",
		"email": "plugin-signer@opencapital.dev",
		"sub":   "ChU...local",
	}
	got := identityFromClaims("http://dex:5556/dex", claims)
	if got != "http://dex:5556/dex|plugin-signer@opencapital.dev" {
		t.Fatalf("got %q", got)
	}
}

func TestSubFallback(t *testing.T) {
	claims := map[string]any{"iss": "http://x", "sub": "abc"}
	if got := identityFromClaims("http://x", claims); got != "http://x|abc" {
		t.Fatalf("got %q", got)
	}
}

// job_workflow_ref wins over email when both are present.
func TestJobWorkflowRefPriorityOverEmail(t *testing.T) {
	claims := map[string]any{
		"iss":              "https://token.actions.githubusercontent.com",
		"job_workflow_ref": "org/repo/.github/workflows/release.yml@refs/heads/main",
		"email":            "user@example.com",
	}
	got := identityFromClaims("https://token.actions.githubusercontent.com", claims)
	want := "https://token.actions.githubusercontent.com|org/repo/.github/workflows/release.yml@refs/heads/main"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Synthetic-JWKS Verify test (httptest server — no external network)
// ---------------------------------------------------------------------------

func TestVerify_EmailIdentity(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.NewServeMux()) // placeholder — replaced below
	srv.Close()

	// We need the test server URL before building it; use a two-step approach.
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	const kid = "k1"
	nB64 := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	eB64 := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	jwkDoc := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": nB64, "e": eB64},
		},
	}

	const audience = "sigstore"
	var issuer string

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":   issuer,
			"jwks_uri": issuer + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jwkDoc)
	})
	realSrv := httptest.NewServer(mux)
	t.Cleanup(realSrv.Close)

	issuer = realSrv.URL

	mapClaims := jwt.MapClaims{
		"iss":   issuer,
		"aud":   jwt.ClaimStrings{audience},
		"sub":   "local|abc",
		"email": "plugin-signer@opencapital.dev",
		"exp":   time.Now().Add(5 * time.Minute).Unix(),
		"iat":   time.Now().Unix(),
	}
	tokObj := jwt.NewWithClaims(jwt.SigningMethodRS256, mapClaims)
	tokObj.Header["kid"] = kid
	rawToken, err := tokObj.SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	v := New([]string{issuer}, audience)
	v.http = realSrv.Client()

	identity, err := v.Verify(ctx, rawToken)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	want := issuer + "|plugin-signer@opencapital.dev"
	if identity != want {
		t.Fatalf("got %q want %q", identity, want)
	}
}

func TestVerify_GitHubWorkflowRef(t *testing.T) {
	ctx := context.Background()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	const kid = "k2"
	nB64 := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	eB64 := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	jwkDoc := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": nB64, "e": eB64},
		},
	}
	const audience = "sigstore"
	var issuer string

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jwkDoc)
	})
	realSrv := httptest.NewServer(mux)
	t.Cleanup(realSrv.Close)

	issuer = realSrv.URL

	const workflowRef = "acme/plugin/.github/workflows/release.yml@refs/tags/v1.0.0"
	mapClaims := jwt.MapClaims{
		"iss":              issuer,
		"aud":              jwt.ClaimStrings{audience},
		"sub":              "repo:acme/plugin:ref:refs/tags/v1.0.0",
		"job_workflow_ref": workflowRef,
		"exp":              time.Now().Add(5 * time.Minute).Unix(),
		"iat":              time.Now().Unix(),
	}
	tokObj := jwt.NewWithClaims(jwt.SigningMethodRS256, mapClaims)
	tokObj.Header["kid"] = kid
	rawToken, err := tokObj.SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	v := New([]string{issuer}, audience)
	v.http = realSrv.Client()

	identity, err := v.Verify(ctx, rawToken)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	want := issuer + "|" + workflowRef
	if identity != want {
		t.Fatalf("got %q want %q", identity, want)
	}
}

func TestVerify_UnknownIssuerRejected(t *testing.T) {
	// Verifier with no accepted issuers should reject any token without
	// making any network call.
	v := New([]string{"https://trusted.example.com"}, "sigstore")

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	mapClaims := jwt.MapClaims{
		"iss": "https://attacker.example.com",
		"aud": jwt.ClaimStrings{"sigstore"},
		"sub": "evil",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, mapClaims)
	tok.Header["kid"] = "any"
	raw, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	_, err = v.Verify(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for untrusted issuer, got nil")
	}
}

func TestVerify_WrongAudienceRejected(t *testing.T) {
	ctx := context.Background()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	const kid = "k3"
	nB64 := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	eB64 := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	jwkDoc := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": nB64, "e": eB64},
		},
	}
	var issuer string

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jwkDoc)
	})
	realSrv := httptest.NewServer(mux)
	t.Cleanup(realSrv.Close)
	issuer = realSrv.URL

	mapClaims := jwt.MapClaims{
		"iss": issuer,
		"aud": jwt.ClaimStrings{"wrong-audience"},
		"sub": "some-sub",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	tokObj := jwt.NewWithClaims(jwt.SigningMethodRS256, mapClaims)
	tokObj.Header["kid"] = kid
	rawToken, err := tokObj.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	v := New([]string{issuer}, "sigstore")
	v.http = realSrv.Client()

	_, err = v.Verify(ctx, rawToken)
	if err == nil {
		t.Fatal("expected error for wrong audience, got nil")
	}
}

func TestVerify_ExpiredTokenRejected(t *testing.T) {
	ctx := context.Background()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	const kid = "k4"
	nB64 := base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes())
	eB64 := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes())
	jwkDoc := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": nB64, "e": eB64},
		},
	}
	var issuer string

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jwkDoc)
	})
	realSrv := httptest.NewServer(mux)
	t.Cleanup(realSrv.Close)
	issuer = realSrv.URL

	mapClaims := jwt.MapClaims{
		"iss": issuer,
		"aud": jwt.ClaimStrings{"sigstore"},
		"sub": "some-sub",
		// expired 1 hour ago
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	}
	tokObj := jwt.NewWithClaims(jwt.SigningMethodRS256, mapClaims)
	tokObj.Header["kid"] = kid
	rawToken, err := tokObj.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	v := New([]string{issuer}, "sigstore")
	v.http = realSrv.Client()

	_, err = v.Verify(ctx, rawToken)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}
