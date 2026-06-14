// Package viewschema introspects RisingWave view columns from information_schema,
// replacing the hand-maintained catalog's per-entity Columns/Numerics declarations.
// Results are cached after the first load; call Load or Refresh to (re)populate.
package viewschema

import (
	"context"
	"fmt"
	"sync"

	"github.com/portfolio-management/read-gateway/internal/rw"
)

// Column is one column in a view. Numeric is true for SQL numeric types
// (integer family, numeric/decimal, float family).
type Column struct {
	Name    string
	Numeric bool
}

// Querier is the subset of rw.Reader used by Cache — satisfied by *rw.Reader.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (rw.Rows, error)
}

// Cache holds the introspected column sets for one or more views, keyed by
// view name. Thread-safe.
type Cache struct {
	q    Querier
	mu   sync.RWMutex
	data map[string][]Column
}

// New returns a Cache backed by q. Call Load or Refresh before Columns.
func New(q Querier) *Cache {
	return &Cache{q: q, data: make(map[string][]Column)}
}

// numericDataTypes is the set of information_schema data_type strings that
// RisingWave reports for numeric-family columns. Based on RW's pg-compatible
// information_schema; extended with common aliases RW surfaces.
var numericDataTypes = map[string]bool{
	"smallint":         true,
	"integer":          true,
	"bigint":           true,
	"numeric":          true,
	"decimal":          true,
	"real":             true,
	"double precision": true,
	"int2":             true,
	"int4":             true,
	"int8":             true,
	"float4":           true,
	"float8":           true,
	"float":            true,
	"int":              true,
	"serial":           true,
	"bigserial":        true,
}

const schemaQuery = `
SELECT column_name, data_type
FROM information_schema.columns
WHERE table_name = $1
ORDER BY ordinal_position`

// Load populates the cache for the given view names. Existing entries are
// replaced. Returns the first error; remaining views are skipped.
func (c *Cache) Load(ctx context.Context, views ...string) error {
	built := make(map[string][]Column, len(views))
	for _, v := range views {
		cols, err := c.fetch(ctx, v)
		if err != nil {
			return err
		}
		built[v] = cols
	}
	c.mu.Lock()
	for k, v := range built {
		c.data[k] = v
	}
	c.mu.Unlock()
	return nil
}

// Refresh reloads every view currently in the cache.
func (c *Cache) Refresh(ctx context.Context) error {
	c.mu.RLock()
	views := make([]string, 0, len(c.data))
	for v := range c.data {
		views = append(views, v)
	}
	c.mu.RUnlock()
	return c.Load(ctx, views...)
}

// Columns returns the cached column set for view. An unknown view (never
// loaded) or one with no rows in information_schema returns an error.
func (c *Cache) Columns(view string) ([]Column, error) {
	c.mu.RLock()
	cols, ok := c.data[view]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("viewschema: unknown view %q (not loaded)", view)
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("viewschema: view %q has no columns in information_schema (view does not exist?)", view)
	}
	return cols, nil
}

func (c *Cache) fetch(ctx context.Context, view string) ([]Column, error) {
	res, err := c.q.Query(ctx, schemaQuery, view)
	if err != nil {
		return nil, fmt.Errorf("viewschema: query columns for %q: %w", view, err)
	}
	cols := make([]Column, 0, len(res.Rows))
	for _, row := range res.Rows {
		if len(row) < 2 {
			continue
		}
		name, _ := row[0].(string)
		dtype, _ := row[1].(string)
		if name == "" {
			continue
		}
		cols = append(cols, Column{Name: name, Numeric: numericDataTypes[dtype]})
	}
	return cols, nil
}
