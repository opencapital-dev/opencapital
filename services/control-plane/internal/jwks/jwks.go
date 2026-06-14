// Package jwks implements a minimal JWKS client that fetches an upstream JWK
// Set, caches it, and resolves kid→PublicKey lookups for JWT verification.
// Supports the two algorithms Grafana and this control plane care about:
// RS256 (Grafana's default) and ES256 (this control plane's own JWKS).
//
// A kid miss triggers a refresh (capped to once per minRefreshInterval) so
// new signing keys are picked up promptly; a periodic background refresh
// keeps the cache warm at refreshInterval.
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
	"sync"
	"time"
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

	// EC
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`

	// RSA
	N string `json:"n,omitempty"`
	E string `json:"e,omitempty"`
}

type jwks struct {
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
func WithRefreshInterval(d time.Duration) Option { return func(j *Client) { j.refreshInt = d } }

// New constructs a JWKS client. The first Refresh is not performed; call it
// at startup so the cache is warm before verifies arrive.
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
	var doc jwks
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

// Run starts a background refresh loop until ctx is cancelled. Errors are
// surfaced via the optional callback; nil callback discards them.
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

// KeyFunc returns a jwt.Keyfunc-compatible callback that looks up by kid and
// refreshes the cache on a kid miss (rate-limited by minRefreshInterval).
// Passed to jwt.Parse via jwt.WithKeyFunc isn't a thing — jwt.Parse takes a
// Keyfunc directly. Use this with `parser.Parse(token, client.KeyFunc(ctx))`.
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
