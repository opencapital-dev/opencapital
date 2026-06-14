package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// adapt wraps a pgxmock.PgxPoolIface so it satisfies our PgxQuerier
// interface, which only declares QueryRow.
type adapter struct {
	pool pgxmock.PgxPoolIface
}

func (a adapter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return a.pool.QueryRow(ctx, sql, args...)
}

func newStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock new: %v", err)
	}
	return New(adapter{pool: mock}), mock
}

func TestLookupPortfolioOrg_OK(t *testing.T) {
	s, mock := newStore(t)
	defer mock.Close()
	portfolio := uuid.New()
	org := uuid.New()
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(portfolio).
		WillReturnRows(pgxmock.NewRows([]string{"org_id"}).AddRow(org))

	got, err := s.LookupPortfolioOrg(context.Background(), portfolio)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if got != org {
		t.Fatalf("expected %v, got %v", org, got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLookupPortfolioOrg_Missing(t *testing.T) {
	s, mock := newStore(t)
	defer mock.Close()
	portfolio := uuid.New()
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(portfolio).
		WillReturnError(pgx.ErrNoRows)

	_, err := s.LookupPortfolioOrg(context.Background(), portfolio)
	if !errors.Is(err, ErrPortfolioNotFound) {
		t.Fatalf("expected ErrPortfolioNotFound, got %v", err)
	}
}

func TestLookupPortfolioOrg_DBError(t *testing.T) {
	s, mock := newStore(t)
	defer mock.Close()
	portfolio := uuid.New()
	mock.ExpectQuery("SELECT org_id FROM portfolios").
		WithArgs(portfolio).
		WillReturnError(errors.New("connection refused"))

	_, err := s.LookupPortfolioOrg(context.Background(), portfolio)
	if err == nil || errors.Is(err, ErrPortfolioNotFound) {
		t.Fatalf("expected transport error, got %v", err)
	}
}
