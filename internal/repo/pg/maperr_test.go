package pg

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/robertjndw/gotero/internal/repo"
)

func TestMapErr(t *testing.T) {
	pgErr := func(code string) error { return &pgconn.PgError{Code: code} }
	tests := []struct {
		name string
		err  error
		want error // nil means the error must pass through untouched
	}{
		{"unique", pgErr("23505"), repo.ErrConflict},
		{"foreign key", pgErr("23503"), repo.ErrNotFound},
		{"no rows", pgx.ErrNoRows, repo.ErrNotFound},
		{"lock not available", pgErr("55P03"), repo.ErrBusy},
		{"serialization failure", pgErr("40001"), repo.ErrBusy},
		{"deadlock", pgErr("40P01"), repo.ErrBusy},
		{"query canceled", pgErr("57014"), repo.ErrBusy},
		{"too many connections", pgErr("53300"), repo.ErrBusy},
		{"admin shutdown", pgErr("57P01"), repo.ErrBusy},
		{"crash shutdown", pgErr("57P02"), repo.ErrBusy},
		{"cannot connect now", pgErr("57P03"), repo.ErrBusy},
		{"connection failure", pgErr("08006"), repo.ErrBusy},
		{"connection does not exist", pgErr("08003"), repo.ErrBusy},
		{"wrapped busy code", fmt.Errorf("op: %w", pgErr("40001")), repo.ErrBusy},
		{"syntax error", pgErr("42601"), nil},
		{"check violation", pgErr("23514"), nil},
		{"plain error", errors.New("boom"), nil},
		{"client cancel", context.Canceled, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapErr(tt.err)
			if tt.want == nil {
				assert.Equal(t, tt.err, got)
				return
			}
			assert.ErrorIs(t, got, tt.want)
			assert.ErrorIs(t, got, tt.err, "the cause stays reachable for logging")
			assert.Equal(t, got, mapErr(got), "mapErr is idempotent")
		})
	}
}
