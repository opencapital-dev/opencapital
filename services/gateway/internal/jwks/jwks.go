// Package jwks re-exports the shared JWKS client from lib/jwks so the
// gateway's internal import paths remain unchanged.
package jwks

import shared "github.com/portfolio-management/jwks"

// Client is the shared JWKS client.
type Client = shared.Client

// Option is a functional option for Client.
type Option = shared.Option

// WithHTTPClient overrides the default http.Client.
var WithHTTPClient = shared.WithHTTPClient

// WithRefreshInterval sets the background refresh cadence.
var WithRefreshInterval = shared.WithRefreshInterval

// New constructs a JWKS client. Call Refresh once at startup so the cache is
// warm before the first verify.
func New(url string, opts ...Option) *Client { return shared.New(url, opts...) }
