package rw

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Rows struct {
	Columns []string
	Rows    [][]any
}

type Reader struct{ pool *pgxpool.Pool }

// New dials RisingWave with the pg-wire SIMPLE query protocol. RisingWave's
// extended-protocol (Parse/Bind prepared-statement) path hangs on complex
// multi-CTE template queries and mis-infers params against its varchar
// org_id/portfolio_id columns; simple protocol inlines params as escaped
// text — matching the psql path that runs these queries in milliseconds. The
// only cost is no prepared-statement caching, irrelevant at dashboard QPS.
func New(ctx context.Context, dsn string) (*Reader, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Reader{pool: p}, nil
}
func (r *Reader) Close() { r.pool.Close() }

func (r *Reader) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return Rows{}, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	out := Rows{Columns: make([]string, len(fds))}
	for i, f := range fds {
		out.Columns[i] = string(f.Name)
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return Rows{}, err
		}
		out.Rows = append(out.Rows, vals)
	}
	return out, rows.Err()
}
