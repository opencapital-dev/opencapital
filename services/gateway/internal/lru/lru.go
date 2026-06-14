// Package lru is the gateway's in-process map of portfolio_id -> org_id.
//
// Per ADR-0034 it has no capacity cap: the gateway holds every row of
// control_db.portfolios in memory after boot pre-warm. At ~128 bytes
// per entry (UUID + UUID + Go map overhead) one million portfolios is
// ~128 MB resident, which is cheap compared to a per-request Postgres
// hop. The map is the hot path; the read replica is only ever touched
// on a true miss (race against a brand-new portfolio whose
// write-through notification has not arrived yet) and on boot
// pre-warm.
//
// Name is "lru" for historical reasons in the design doc; the runtime
// behaviour is a fully-populated cache with no eviction.
package lru

import (
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// Cache is concurrent-safe; reads do not block other reads. sync.Map is
// chosen over a guarded map because the access pattern is
// many-readers-rare-writers (Put fires only at boot pre-warm time and
// on control-plane write-through), where sync.Map's two-store design
// outperforms a single mutex.
type Cache struct {
	m    sync.Map // string portfolio_id -> uuid.UUID org_id
	size atomic.Int64
}

// New returns an empty Cache. The cache MUST be primed via Put before
// it serves traffic; the gateway gates /readyz on pre-warm completion.
func New() *Cache { return &Cache{} }

// Get returns the cached org_id for portfolioID and a present bit.
// A missing portfolio_id and a portfolio_id that has not yet been
// pre-warmed both return (uuid.Nil, false); callers fall back to the
// replica.
func (c *Cache) Get(portfolioID string) (uuid.UUID, bool) {
	v, ok := c.m.Load(portfolioID)
	if !ok {
		return uuid.Nil, false
	}
	return v.(uuid.UUID), true
}

// Put writes the mapping unconditionally. Subsequent Gets for the
// same key see the new value. Returns true when the key was not
// previously present (so callers can drive a Prometheus counter
// without re-reading the map). The size gauge counts unique keys
// inserted; updates to an existing key do not bump it.
func (c *Cache) Put(portfolioID string, orgID uuid.UUID) bool {
	if _, loaded := c.m.LoadOrStore(portfolioID, orgID); loaded {
		c.m.Store(portfolioID, orgID)
		return false
	}
	c.size.Add(1)
	return true
}

// Size returns the count of unique portfolio_ids in the cache. Used
// to surface gateway_lru_size as a Prometheus gauge and to log the
// pre-warm row count.
func (c *Cache) Size() int64 { return c.size.Load() }
