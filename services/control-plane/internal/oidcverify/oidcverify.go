// Package oidcverify authenticates a publisher's OIDC token and maps its
// claims to the "<issuer>|<SAN>" identity string consumed by the publishers
// allowlist (internal/publishers). It is used by the push-auth broker
// (Task 15) to authorise registry-push requests.
//
// Accepted issuers are given at construction time; the verifier fetches each
// issuer's JWKS URI lazily via its OpenID Connect discovery document and caches
// one jwks.Client per issuer. An unknown issuer is rejected before any network
// request is made (prevents attacker-controlled JWKS discovery URLs).
package oidcverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/portfolio-management/control-plane/internal/jwks"
)

// Verifier verifies an OIDC token and returns the publisher identity.
// Safe for concurrent use.
type Verifier struct {
	// accepted issuers (set for O(1) lookup)
	issuers  map[string]struct{}
	audience string

	mu      sync.Mutex
	clients map[string]*jwks.Client // issuer -> JWKS client (lazy)

	http *http.Client
}

// New constructs a Verifier. issuers is the set of accepted OIDC issuers
// (e.g. ["http://dex:5556/dex", "https://token.actions.githubusercontent.com"]).
// audience is the required token audience (e.g. "sigstore").
func New(issuers []string, audience string) *Verifier {
	m := make(map[string]struct{}, len(issuers))
	for _, iss := range issuers {
		iss = strings.TrimRight(iss, "/")
		if iss != "" {
			m[iss] = struct{}{}
		}
	}
	return &Verifier{
		issuers:  m,
		audience: audience,
		clients:  make(map[string]*jwks.Client),
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Verify validates the OIDC token's signature, issuer, audience, and expiry,
// then returns the "<issuer>|<SAN>" publisher identity derived from its claims.
//
// Validation order:
//  1. Parse token header+payload without verifying signature to read "iss".
//  2. Reject if "iss" is not in the accepted set (prevents attacker-controlled
//     JWKS discovery URLs from ever being fetched).
//  3. Discover (and cache) the issuer's JWKS URI via OpenID Connect discovery.
//  4. Verify signature (kid → public key lookup via cached jwks.Client),
//     issuer, audience, and expiry using golang-jwt/jwt/v5.
//  5. Map claims → identity string via identityFromClaims.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (string, error) {
	// Step 1 — read iss without verifying the signature.
	parser := jwt.NewParser()
	unverified, _, err := parser.ParseUnverified(rawToken, jwt.MapClaims{})
	if err != nil {
		return "", fmt.Errorf("oidcverify: parse unverified: %w", err)
	}
	claims, ok := unverified.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("oidcverify: unexpected claims type")
	}
	iss, _ := claims["iss"].(string)
	iss = strings.TrimRight(iss, "/")
	if iss == "" {
		return "", errors.New("oidcverify: missing iss claim")
	}

	// Step 2 — issuer allowlist check (before any network I/O).
	if _, accepted := v.issuers[iss]; !accepted {
		return "", fmt.Errorf("oidcverify: issuer %q not accepted", iss)
	}

	// Step 3 — get (or lazily create) the JWKS client for this issuer.
	client, err := v.jwksClient(ctx, iss)
	if err != nil {
		return "", fmt.Errorf("oidcverify: jwks for %s: %w", iss, err)
	}

	// Step 4 — verify the token fully.
	verifyParser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(iss),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)
	keyFunc := client.KeyFunc(ctx)
	tok, err := verifyParser.Parse(rawToken, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("oidcverify: missing kid header")
		}
		return keyFunc(kid)
	})
	if err != nil {
		return "", fmt.Errorf("oidcverify: verify: %w", err)
	}
	if !tok.Valid {
		return "", errors.New("oidcverify: token not valid")
	}

	// Step 5 — map verified claims to publisher identity.
	verifiedClaims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("oidcverify: unexpected claims type after verify")
	}
	return identityFromClaims(iss, map[string]any(verifiedClaims)), nil
}

// jwksClient returns (or lazily creates) the jwks.Client for the given issuer.
// The JWKS URI is discovered via the issuer's OpenID Connect discovery document.
func (v *Verifier) jwksClient(ctx context.Context, issuer string) (*jwks.Client, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.clients[issuer]; ok {
		return c, nil
	}
	jwksURI, err := v.discoverJWKSURI(ctx, issuer)
	if err != nil {
		return nil, err
	}
	c := jwks.New(jwksURI, jwks.WithHTTPClient(v.http))
	if err := c.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("initial jwks refresh: %w", err)
	}
	v.clients[issuer] = c
	return c, nil
}

// oidcDiscovery is the subset of the OpenID Connect discovery document we need.
type oidcDiscovery struct {
	JWKSURI string `json:"jwks_uri"`
}

// discoverJWKSURI fetches <issuer>/.well-known/openid-configuration and
// returns the jwks_uri field. Must be called with v.mu held.
func (v *Verifier) discoverJWKSURI(ctx context.Context, issuer string) (string, error) {
	discoveryURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", fmt.Errorf("build discovery request: %w", err)
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery %s: status %d", discoveryURL, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read discovery: %w", err)
	}
	var doc oidcDiscovery
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parse discovery: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("discovery %s: jwks_uri missing", discoveryURL)
	}
	return doc.JWKSURI, nil
}

// identityFromClaims maps a verified token's claims to the "<issuer>|<SAN>"
// identity string used by the publishers allowlist.
//
// Priority:
//  1. job_workflow_ref — GitHub Actions OIDC tokens carry this claim.
//  2. email — Dex (local stack) issues tokens with an email claim.
//  3. sub — universal fallback.
func identityFromClaims(issuer string, claims map[string]any) string {
	if ref, ok := claims["job_workflow_ref"].(string); ok && ref != "" {
		return issuer + "|" + ref
	}
	if email, ok := claims["email"].(string); ok && email != "" {
		return issuer + "|" + email
	}
	sub, _ := claims["sub"].(string)
	return issuer + "|" + sub
}
