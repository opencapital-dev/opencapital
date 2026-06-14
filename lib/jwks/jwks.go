// Package jwks fetches and caches a JWK Set (the control-plane signing keys)
// and resolves kid -> public key lookups for session-JWT verification.
//
// This is the shared implementation used by the write gateway and read gateway.
// Each service imports it via a replace directive in its go.mod.
//
// Algorithms: ES256 for the control plane's own keys, RS256 forward-compat.
// kid-miss semantics: on an unknown kid, refresh the cache once (rate-limited
// to minRefreshInterval) before returning an error.
package jwks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	defaultRefreshInterval    = 5 * time.Minute
	defaultMinRefreshInterval = 30 * time.Second
)

type jwk struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`

	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`

	N string `json:"n,omitempty"`
	E string `json:"e,omitempty"`
}

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

// Client fetches and caches a JWKS from a URL. Safe for concurrent use.
type Client struct {
	url        string
	http       *http.Client
	refreshInt time.Duration
	minRefresh time.Duration

	mu          sync.RWMutex
	keys        map[string]any // kid -> *rsa.PublicKey | *ecdsa.PublicKey
	lastRefresh time.Time
}

// Option mutates client config.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client.
func WithHTTPClient(c *http.Client) Option { return func(j *Client) { j.http = c } }

// WithRefreshInterval sets the background refresh cadence.
func WithRefreshInterval(d time.Duration) Option {
	return func(j *Client) {
		if d > 0 {
			j.refreshInt = d
		}
	}
}

// New constructs a JWKS client. Call Refresh once at startup so the cache is
// warm before the first verify.
func New(url string, opts ...Option) *Client {
	c := &Client{
		url:        url,
		http:       &http.Client{Timeout: 5 * time.Second},
		refreshInt: defaultRefreshInterval,
		minRefresh: defaultMinRefreshInterval,
		keys:       map[string]any{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Refresh fetches the upstream JWK Set and replaces the cache.
func (c *Client) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks %s: status %d", c.url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read jwks: %w", err)
	}
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("parse jwks: %w", err)
	}

	out := make(map[string]any, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kid == "" {
			continue
		}
		key, err := jwkToPublic(k)
		if err != nil {
			return fmt.Errorf("jwk %s: %w", k.Kid, err)
		}
		out[k.Kid] = key
	}
	c.mu.Lock()
	c.keys = out
	c.lastRefresh = time.Now()
	c.mu.Unlock()
	return nil
}

// Run starts a background refresh loop until ctx is cancelled.
func (c *Client) Run(ctx context.Context, onError func(error)) {
	t := time.NewTicker(c.refreshInt)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Refresh(ctx); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

// Fresh reports whether the cache has been populated within the refresh
// interval. Used by /readyz.
func (c *Client) Fresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastRefresh.IsZero() || len(c.keys) == 0 {
		return false
	}
	// Stale if we haven't refreshed in twice the interval.
	return time.Since(c.lastRefresh) < 2*c.refreshInt
}

// KeyFunc returns a callback that looks up a key by kid, refreshing the cache
// once on miss (rate-limited by minRefreshInterval).
func (c *Client) KeyFunc(ctx context.Context) func(kid string) (any, error) {
	return func(kid string) (any, error) {
		c.mu.RLock()
		key, ok := c.keys[kid]
		last := c.lastRefresh
		c.mu.RUnlock()
		if ok {
			return key, nil
		}
		if time.Since(last) < c.minRefresh {
			return nil, fmt.Errorf("unknown kid %q (recent refresh)", kid)
		}
		if err := c.Refresh(ctx); err != nil {
			return nil, fmt.Errorf("refresh on miss: %w", err)
		}
		c.mu.RLock()
		key, ok = c.keys[kid]
		c.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("unknown kid %q (after refresh)", kid)
		}
		return key, nil
	}
}

// VerifyOrgPlugin verifies the bearer (same checks as VerifyOrg) and returns
// the org_id and plugin_id claims. plugin_id is "" when the token carries none.
func (c *Client) VerifyOrgPlugin(ctx context.Context, bearer, issuer string) (uuid.UUID, string, error) {
	tokenStr := strings.TrimPrefix(bearer, "Bearer ")
	if tokenStr == "" {
		return uuid.UUID{}, "", errors.New("empty bearer token")
	}

	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	keyFunc := c.KeyFunc(ctx)
	tok, err := parser.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		return keyFunc(kid)
	})
	if err != nil {
		return uuid.UUID{}, "", fmt.Errorf("jwt verify: %w", err)
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return uuid.UUID{}, "", errors.New("unexpected claims type")
	}
	orgStr, _ := mc["org_id"].(string)
	orgID, err := uuid.Parse(orgStr)
	if err != nil {
		return uuid.UUID{}, "", fmt.Errorf("org_id claim not a valid UUID: %w", err)
	}
	pluginID, _ := mc["plugin_id"].(string)
	return orgID, pluginID, nil
}

// VerifyOrg verifies a Bearer JWT against the JWKS and returns the org_id
// claim as a UUID. The bearer argument may optionally carry a "Bearer " prefix.
//
// Checks performed (all must pass):
//   - Signature valid per JWKS keys (RS256 or ES256)
//   - iss claim equals issuer
//   - Token not expired (exp required)
//   - org_id claim is a valid UUID
//
// Returns an error on any failure: bad signature, wrong issuer, missing/invalid
// org_id, or expired token.
func (c *Client) VerifyOrg(ctx context.Context, bearer, issuer string) (uuid.UUID, error) {
	org, _, err := c.VerifyOrgPlugin(ctx, bearer, issuer)
	return org, err
}

func jwkToPublic(k jwk) (any, error) {
	switch k.Kty {
	case "RSA":
		return rsaFromJWK(k)
	case "EC":
		return ecFromJWK(k)
	default:
		return nil, fmt.Errorf("unsupported kty %q", k.Kty)
	}
}

func rsaFromJWK(k jwk) (*rsa.PublicKey, error) {
	if k.N == "" || k.E == "" {
		return nil, errors.New("RSA jwk missing n/e")
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, errors.New("RSA exponent zero")
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

func ecFromJWK(k jwk) (*ecdsa.PublicKey, error) {
	if k.Crv != "P-256" {
		return nil, fmt.Errorf("unsupported EC curve %q", k.Crv)
	}
	if k.X == "" || k.Y == "" {
		return nil, errors.New("EC jwk missing x/y")
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("decode x: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("decode y: %w", err)
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}
