package jwks_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/portfolio-management/jwks"
)

// ---- test helpers -----------------------------------------------------------

// testSigner holds a P-256 key whose public half the test JWKS server returns.
type testSigner struct {
	priv *ecdsa.PrivateKey
	kid  string
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return &testSigner{priv: k, kid: "test-kid"}
}

// jwksJSON returns the JWKS document the in-memory server publishes.
func (s *testSigner) jwksJSON() string {
	xb := s.priv.PublicKey.X.Bytes()
	yb := s.priv.PublicKey.Y.Bytes()
	pad := func(b []byte) []byte {
		if len(b) == 32 {
			return b
		}
		out := make([]byte, 32)
		copy(out[32-len(b):], b)
		return out
	}
	return fmt.Sprintf(`{"keys":[{"kty":"EC","crv":"P-256","kid":%q,"use":"sig","alg":"ES256","x":%q,"y":%q}]}`,
		s.kid,
		b64url(pad(xb)),
		b64url(pad(yb)),
	)
}

func b64url(b []byte) string {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		end := i + 3
		if end > len(b) {
			end = len(b)
		}
		chunk := b[i:end]
		v := uint32(0)
		for _, c := range chunk {
			v = v<<8 | uint32(c)
		}
		v <<= uint32((3 - len(chunk)) * 8)
		out.WriteByte(enc[(v>>18)&0x3f])
		out.WriteByte(enc[(v>>12)&0x3f])
		if len(chunk) >= 2 {
			out.WriteByte(enc[(v>>6)&0x3f])
		}
		if len(chunk) == 3 {
			out.WriteByte(enc[v&0x3f])
		}
	}
	return out.String()
}

// makeToken mints a JWT with the supplied claims signed by this signer.
func (s *testSigner) makeToken(t *testing.T, issuer string, orgID uuid.UUID, lifetime time.Duration) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":    issuer,
		"sub":    "user-1",
		"org_id": orgID.String(),
		"iat":    time.Now().Unix(),
		"exp":    time.Now().Add(lifetime).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// newClient builds a jwks.Client pointed at an in-memory JWKS server backed
// by signer and pre-warms it.
func newClient(t *testing.T, signer *testSigner) (*jwks.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, signer.jwksJSON())
	}))
	c := jwks.New(srv.URL)
	if err := c.Refresh(context.Background()); err != nil {
		srv.Close()
		t.Fatalf("refresh: %v", err)
	}
	return c, srv.Close
}

// ---- Refresh / Fresh tests --------------------------------------------------

func TestRefresh_PopulatesCache(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	if !c.Fresh() {
		t.Fatal("expected Fresh() true after Refresh")
	}
}

func TestRefresh_BadURL(t *testing.T) {
	c := jwks.New("http://127.0.0.1:0/no-such-server")
	err := c.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable URL")
	}
}

// ---- JWKS doc parsing tests -------------------------------------------------

func TestJWKS_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()

	c := jwks.New(srv.URL)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected parse error for invalid JSON")
	}
}

func TestJWKS_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := jwks.New(srv.URL)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestJWKS_EmptyKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"keys":[]}`)
	}))
	defer srv.Close()

	c := jwks.New(srv.URL)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty keys => not fresh (no keys populated).
	if c.Fresh() {
		t.Fatal("expected Fresh() false when no keys")
	}
}

func TestJWKS_UnsupportedKty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"keys": []map[string]any{
				{"kty": "OKP", "kid": "k1", "alg": "EdDSA"},
			},
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	c := jwks.New(srv.URL)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected error for unsupported kty")
	}
}

// ---- VerifyOrg tests --------------------------------------------------------

func TestVerifyOrg_HappyPath(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	orgID := uuid.New()
	tok := signer.makeToken(t, "control-plane", orgID, time.Hour)

	got, err := c.VerifyOrg(context.Background(), tok, "control-plane")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != orgID {
		t.Fatalf("org_id mismatch: got %s, want %s", got, orgID)
	}
}

func TestVerifyOrg_BearerPrefix(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	orgID := uuid.New()
	tok := signer.makeToken(t, "control-plane", orgID, time.Hour)

	// Verify that "Bearer <token>" is accepted (prefix stripped).
	got, err := c.VerifyOrg(context.Background(), "Bearer "+tok, "control-plane")
	if err != nil {
		t.Fatalf("unexpected error with Bearer prefix: %v", err)
	}
	if got != orgID {
		t.Fatalf("org_id mismatch: got %s, want %s", got, orgID)
	}
}

func TestVerifyOrg_WrongIssuer(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	tok := signer.makeToken(t, "wrong-issuer", uuid.New(), time.Hour)
	_, err := c.VerifyOrg(context.Background(), tok, "control-plane")
	if err == nil {
		t.Fatal("expected error for wrong issuer, got nil")
	}
}

func TestVerifyOrg_WrongSigningKey(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	// Token signed by a different key not in the JWKS.
	otherSigner := newTestSigner(t)
	tok := otherSigner.makeToken(t, "control-plane", uuid.New(), time.Hour)

	_, err := c.VerifyOrg(context.Background(), tok, "control-plane")
	if err == nil {
		t.Fatal("expected error for wrong signing key, got nil")
	}
}

func TestVerifyOrg_Expired(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	tok := signer.makeToken(t, "control-plane", uuid.New(), -1*time.Minute)
	_, err := c.VerifyOrg(context.Background(), tok, "control-plane")
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestVerifyOrg_MissingOrgID(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	// Mint a token with no org_id claim.
	claims := jwt.MapClaims{
		"iss": "control-plane",
		"sub": "user-1",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = signer.kid
	signed, err := tok.SignedString(signer.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	_, err = c.VerifyOrg(context.Background(), signed, "control-plane")
	if err == nil {
		t.Fatal("expected error for missing org_id, got nil")
	}
}

func TestVerifyOrg_InvalidOrgIDFormat(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	// Mint a token with org_id set to a non-UUID string.
	claims := jwt.MapClaims{
		"iss":    "control-plane",
		"sub":    "user-1",
		"org_id": "not-a-uuid",
		"iat":    time.Now().Unix(),
		"exp":    time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = signer.kid
	signed, err := tok.SignedString(signer.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	_, err = c.VerifyOrg(context.Background(), signed, "control-plane")
	if err == nil {
		t.Fatal("expected error for invalid org_id UUID format, got nil")
	}
}

func TestVerifyOrg_EmptyBearer(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	_, err := c.VerifyOrg(context.Background(), "", "control-plane")
	if err == nil {
		t.Fatal("expected error for empty bearer, got nil")
	}
}

// ---- VerifyOrgPlugin tests --------------------------------------------------

// makeTokenWithPlugin mints a JWT that optionally includes a plugin_id claim.
func (s *testSigner) makeTokenWithPlugin(t *testing.T, issuer string, orgID uuid.UUID, lifetime time.Duration, pluginID string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":    issuer,
		"sub":    "user-1",
		"org_id": orgID.String(),
		"iat":    time.Now().Unix(),
		"exp":    time.Now().Add(lifetime).Unix(),
	}
	if pluginID != "" {
		claims["plugin_id"] = pluginID
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestVerifyOrgPlugin_WithPluginID(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	orgID := uuid.New()
	wantPlugin := "my-plugin"
	tok := signer.makeTokenWithPlugin(t, "control-plane", orgID, time.Hour, wantPlugin)

	gotOrg, gotPlugin, err := c.VerifyOrgPlugin(context.Background(), tok, "control-plane")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotOrg != orgID {
		t.Fatalf("org_id mismatch: got %s, want %s", gotOrg, orgID)
	}
	if gotPlugin != wantPlugin {
		t.Fatalf("plugin_id mismatch: got %q, want %q", gotPlugin, wantPlugin)
	}
}

func TestVerifyOrgPlugin_WithoutPluginID(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	orgID := uuid.New()
	tok := signer.makeTokenWithPlugin(t, "control-plane", orgID, time.Hour, "")

	gotOrg, gotPlugin, err := c.VerifyOrgPlugin(context.Background(), tok, "control-plane")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotOrg != orgID {
		t.Fatalf("org_id mismatch: got %s, want %s", gotOrg, orgID)
	}
	if gotPlugin != "" {
		t.Fatalf("expected empty plugin_id, got %q", gotPlugin)
	}
}

func TestVerifyOrgPlugin_BearerPrefix(t *testing.T) {
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	orgID := uuid.New()
	tok := signer.makeTokenWithPlugin(t, "control-plane", orgID, time.Hour, "p1")

	gotOrg, gotPlugin, err := c.VerifyOrgPlugin(context.Background(), "Bearer "+tok, "control-plane")
	if err != nil {
		t.Fatalf("unexpected error with Bearer prefix: %v", err)
	}
	if gotOrg != orgID {
		t.Fatalf("org_id mismatch: got %s, want %s", gotOrg, orgID)
	}
	if gotPlugin != "p1" {
		t.Fatalf("plugin_id mismatch: got %q, want %q", gotPlugin, "p1")
	}
}

func TestVerifyOrgPlugin_DelegatesVerifyOrg(t *testing.T) {
	// Confirm VerifyOrg still works correctly (delegates to VerifyOrgPlugin).
	signer := newTestSigner(t)
	c, cleanup := newClient(t, signer)
	defer cleanup()

	orgID := uuid.New()
	// Token with a plugin_id — VerifyOrg must ignore it and return only orgID.
	tok := signer.makeTokenWithPlugin(t, "control-plane", orgID, time.Hour, "irrelevant-plugin")

	got, err := c.VerifyOrg(context.Background(), tok, "control-plane")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != orgID {
		t.Fatalf("org_id mismatch: got %s, want %s", got, orgID)
	}
}
