package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	jwks "github.com/portfolio-management/jwks"
)

func TestOwns_CachesPortfolios(t *testing.T) {
	org := uuid.New()
	p := uuid.New()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"portfolio_id":"` + p.String() + `","attributes":{"name":"X"}}]`))
	}))
	defer srv.Close()
	o := NewOwnership(srv.URL, time.Minute)
	ok, err := o.Owns(context.Background(), org, "bearer", p)
	if err != nil || !ok {
		t.Fatalf("want owned, got ok=%v err=%v", ok, err)
	}
	bad, _ := o.Owns(context.Background(), org, "bearer", uuid.New())
	if bad {
		t.Fatal("unknown portfolio must not be owned")
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("want 1 control-plane hit (cached), got %d", hits)
	}
}

// ---- Verifier.Identify tests ------------------------------------------------

// authTestSigner is a minimal signing helper for Verifier.Identify tests.
type authTestSigner struct {
	priv *ecdsa.PrivateKey
	kid  string
}

func newAuthTestSigner(t *testing.T) *authTestSigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return &authTestSigner{priv: k, kid: "auth-test-kid"}
}

func b64urlAuth(b []byte) string {
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

func (s *authTestSigner) jwksJSON() string {
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
		s.kid, b64urlAuth(pad(xb)), b64urlAuth(pad(yb)))
}

func (s *authTestSigner) mintToken(t *testing.T, issuer string, orgID uuid.UUID, pluginID string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":    issuer,
		"sub":    "user-1",
		"org_id": orgID.String(),
		"iat":    time.Now().Unix(),
		"exp":    time.Now().Add(time.Hour).Unix(),
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

func newAuthVerifier(t *testing.T, signer *authTestSigner, issuer string) (*Verifier, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, signer.jwksJSON())
	}))
	c := jwks.New(srv.URL)
	if err := c.Refresh(context.Background()); err != nil {
		srv.Close()
		t.Fatalf("jwks refresh: %v", err)
	}
	return NewVerifier(c, issuer), srv.Close
}

func TestIdentify_WithPluginID(t *testing.T) {
	signer := newAuthTestSigner(t)
	v, cleanup := newAuthVerifier(t, signer, "control-plane")
	defer cleanup()

	orgID := uuid.New()
	wantPlugin := "my-plugin"
	tok := signer.mintToken(t, "control-plane", orgID, wantPlugin)

	gotOrg, gotPlugin, err := v.Identify(context.Background(), tok)
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

func TestIdentify_WithoutPluginID(t *testing.T) {
	signer := newAuthTestSigner(t)
	v, cleanup := newAuthVerifier(t, signer, "control-plane")
	defer cleanup()

	orgID := uuid.New()
	tok := signer.mintToken(t, "control-plane", orgID, "")

	gotOrg, gotPlugin, err := v.Identify(context.Background(), tok)
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
