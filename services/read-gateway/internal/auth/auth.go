package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	jwks "github.com/portfolio-management/jwks"
)

// Verifier validates the caller Bearer against the control-plane JWKS and
// extracts the org claim. Mirrors the write gateway.
type Verifier struct {
	jwks   *jwks.Client
	issuer string
}

func NewVerifier(j *jwks.Client, issuer string) *Verifier { return &Verifier{jwks: j, issuer: issuer} }

func (v *Verifier) OrgFromBearer(ctx context.Context, bearer string) (uuid.UUID, error) {
	return v.jwks.VerifyOrg(ctx, bearer, v.issuer)
}

// Identify verifies the bearer and returns (org, plugin_id). plugin_id is ""
// for tokens without the claim.
func (v *Verifier) Identify(ctx context.Context, bearer string) (uuid.UUID, string, error) {
	return v.jwks.VerifyOrgPlugin(ctx, bearer, v.issuer)
}

type portfolio struct {
	PortfolioID string            `json:"portfolio_id"`
	Attributes  map[string]string `json:"attributes"`
}

type cacheEntry struct {
	ids map[uuid.UUID]string // id -> name
	exp time.Time
}

// Ownership fetches + caches the org's portfolios from control-plane and
// answers membership questions. The cached list also feeds the portfolios
// dropdown (Portfolios).
type Ownership struct {
	cpURL string
	ttl   time.Duration
	mu    sync.Mutex
	cache map[uuid.UUID]cacheEntry // org -> portfolios
}

func NewOwnership(cpURL string, ttl time.Duration) *Ownership {
	return &Ownership{cpURL: cpURL, ttl: ttl, cache: map[uuid.UUID]cacheEntry{}}
}

func (o *Ownership) list(ctx context.Context, org uuid.UUID, bearer string) (map[uuid.UUID]string, error) {
	o.mu.Lock()
	if e, ok := o.cache[org]; ok && time.Now().Before(e.exp) {
		o.mu.Unlock()
		return e.ids, nil
	}
	o.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.cpURL+"/portfolios", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control-plane /portfolios: %d", resp.StatusCode)
	}
	var ps []portfolio
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return nil, err
	}
	ids := map[uuid.UUID]string{}
	for _, p := range ps {
		if id, err := uuid.Parse(p.PortfolioID); err == nil {
			ids[id] = p.Attributes["name"]
		}
	}
	o.mu.Lock()
	o.cache[org] = cacheEntry{ids: ids, exp: time.Now().Add(o.ttl)}
	o.mu.Unlock()
	return ids, nil
}

func (o *Ownership) Owns(ctx context.Context, org uuid.UUID, bearer string, p uuid.UUID) (bool, error) {
	ids, err := o.list(ctx, org, bearer)
	if err != nil {
		return false, err
	}
	_, ok := ids[p]
	return ok, nil
}

// Portfolios returns the org's portfolios as id->name for the dropdown.
func (o *Ownership) Portfolios(ctx context.Context, org uuid.UUID, bearer string) (map[uuid.UUID]string, error) {
	return o.list(ctx, org, bearer)
}
