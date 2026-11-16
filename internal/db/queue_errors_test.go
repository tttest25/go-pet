package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapQueueError(t *testing.T) {
	ordinary := errors.New("ordinary")
	missing := &pgconn.PgError{Code: "P0002"}
	invalid := &pgconn.PgError{Code: "22023"}
	other := &pgconn.PgError{Code: "23514"}
	wrapped := fmt.Errorf("wrapped: %w", invalid)
	for _, tc := range []struct {
		name          string
		err, sentinel error
		pg            *pgconn.PgError
		unchanged     bool
	}{
		{"nil", nil, nil, nil, true}, {"ordinary", ordinary, nil, nil, true},
		{"canceled", context.Canceled, nil, nil, true}, {"no rows", pgx.ErrNoRows, pgx.ErrNoRows, nil, true},
		{"missing", missing, pgx.ErrNoRows, missing, false}, {"invalid", invalid, ErrInvalidCallbackStatus, invalid, false},
		{"other SQLSTATE", other, nil, other, true}, {"wrapped", wrapped, ErrInvalidCallbackStatus, invalid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mapQueueError(tc.err)
			if tc.unchanged && got != tc.err {
				t.Fatalf("got %v, want original %v", got, tc.err)
			}
			for _, sentinel := range []error{pgx.ErrNoRows, ErrInvalidCallbackStatus} {
				if errors.Is(got, sentinel) != (tc.sentinel == sentinel) {
					t.Errorf("errors.Is(%v, %v) unexpected", got, sentinel)
				}
			}
			var pgErr *pgconn.PgError
			if errors.As(got, &pgErr) != (tc.pg != nil) || pgErr != tc.pg {
				t.Errorf("PgError = %v, want %v", pgErr, tc.pg)
			}
		})
	}
}
