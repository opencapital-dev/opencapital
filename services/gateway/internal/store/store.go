// Package store wraps the gateway's read-only access to control_db.
//
// The only query the gateway runs against Postgres is the portfolio_id ->
// org_id ownership lookup. It uses a constant-time-shaped query path
// (ADR-0039): the SELECT runs unconditionally on every state-mutating
// portfolio_events request, regardless of whether the JWT and the row agree.
// Both "portfolio missing" and "portfolio owned by a different org" map to
// the same 404 in the HTTP layer, with no early-return branch.
package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PgxQuerier is the pgxpool interface we exercise. Stated as an interface so
// pgxmock can stand in for unit tests without dragging a real Postgres in.
type PgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store wraps the read-only pgxpool. The DSN is expected to point at the
// `gateway_ro` role, which has SELECT on control_db.portfolios and nothing
// else.
type Store struct {
	q PgxQuerier
}

// New returns a Store backed by the supplied querier (pgxpool.Pool or a mock).
func New(q PgxQuerier) *Store { return &Store{q: q} }

// ErrPortfolioNotFound is returned when the portfolio row does not exist OR
// the row exists under a different org_id than the caller's JWT. Callers
// must NOT distinguish the two cases at the HTTP boundary — see ADR-0039 on
// avoiding existence oracles.
var ErrPortfolioNotFound = errors.New("portfolio not found")

// LookupPortfolioOrg runs the miss-path query against the read replica:
// SELECT org_id FROM portfolios WHERE portfolio_id = $1. Returns
// ErrPortfolioNotFound when the row does not exist (the caller will
// surface 404). Other pgx errors are returned verbatim — the HTTP layer
// maps them to 503 to fail closed (ADR-0039) without distinguishing
// "primary down" from "replica lagging" — both are the same outage
// from a caller's perspective.
//
// The ownership comparison (row org vs JWT org) is NOT done here.
// Today's gateway pipeline reads from this method only on LRU miss; the
// LRU itself stores the full mapping and the compare happens at the
// HTTP layer to keep both the hit and miss paths timing-uniform.
func (s *Store) LookupPortfolioOrg(ctx context.Context, portfolioID uuid.UUID) (uuid.UUID, error) {
	var rowOrg uuid.UUID
	err := s.q.QueryRow(ctx,
		`SELECT org_id FROM portfolios WHERE portfolio_id = $1`,
		portfolioID,
	).Scan(&rowOrg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrPortfolioNotFound
		}
		return uuid.Nil, err
	}
	return rowOrg, nil
}

// Ping is a trivial liveness probe for /readyz. Goes through the same pool;
// surfaces a typed error if the pool is dead.
type Pinger interface {
	Ping(ctx context.Context) error
}
