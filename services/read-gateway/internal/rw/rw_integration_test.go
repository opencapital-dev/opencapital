//go:build integration

package rw

import (
	"context"
	"os"
	"testing"
)

func TestReader_Query_RoundTrip(t *testing.T) {
	dsn := os.Getenv("RISINGWAVE_DSN")
	if dsn == "" {
		t.Skip("RISINGWAVE_DSN not set")
	}
	r, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.Query(context.Background(), "SELECT 1 AS x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Columns) != 1 || got.Columns[0] != "x" || len(got.Rows) != 1 {
		t.Fatalf("got %+v", got)
	}
}
