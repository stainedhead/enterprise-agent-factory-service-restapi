package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

func pgErr(code string) error {
	return fmt.Errorf("exec: %w", &pgconn.PgError{Code: code, Message: "m", ConstraintName: "c"})
}

func TestMapError(t *testing.T) {
	plain := errors.New("plain")
	tests := []struct {
		name   string
		err    error
		op     Op
		want   domain.ErrorKind
		reason string
	}{
		{"check", pgErr("23514"), OpWrite, domain.KindValidation, ""},
		{"not null", pgErr("23502"), OpWrite, domain.KindValidation, ""},
		{"data exception", pgErr("22P02"), OpWrite, domain.KindValidation, ""},
		{"invalid parameter", pgErr("22023"), OpWrite, domain.KindValidation, ""},
		{"unique", pgErr("23505"), OpWrite, domain.KindConflict, "duplicate"},
		{"fk on write", pgErr("23503"), OpWrite, domain.KindValidation, ""},
		{"fk on delete", pgErr("23503"), OpDelete, domain.KindConflict, "dependents_exist"},
		{"state", pgErr("55000"), OpWrite, domain.KindConflict, "invalid_state"},
		{"privilege", pgErr("42501"), OpWrite, domain.KindAccessDenied, ""},
		{"no data", pgErr("P0002"), OpWrite, domain.KindInternal, ""},
		{"serialization after retries", pgErr("40001"), OpWrite, domain.KindInternal, ""},
		{"unexpected", pgErr("P0001"), OpWrite, domain.KindInternal, ""},
		{"lock timeout", pgErr("55P03"), OpWrite, domain.KindUnavailable, ""},
		{"statement timeout", pgErr("57014"), OpRead, domain.KindUnavailable, ""},
		{"admin shutdown", pgErr("57P01"), OpRead, domain.KindUnavailable, ""},
		{"connection exception", pgErr("08006"), OpRead, domain.KindUnavailable, ""},
		{"too many connections", pgErr("53300"), OpRead, domain.KindUnavailable, ""},
		{"deadline", context.DeadlineExceeded, OpRead, domain.KindUnavailable, ""},
		{"connect", &pgconn.ConnectError{}, OpRead, domain.KindUnavailable, ""},
		{"no rows", pgx.ErrNoRows, OpRead, domain.KindNotFound, ""},
		{"plain", plain, OpRead, domain.KindInternal, ""},
		{"domain passes", domain.PreconditionFailed(), OpWrite, domain.KindPreconditionFailed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapError(tt.err, tt.op, "team")
			if k := domain.KindOf(got); k != tt.want {
				t.Fatalf("kind = %s, want %s (%v)", k, tt.want, got)
			}
			var de *domain.Error
			if !errors.As(got, &de) {
				t.Fatal("not a domain error")
			}
			if tt.reason != "" && de.Reason != tt.reason {
				t.Fatalf("reason = %q", de.Reason)
			}
			if de.Kind != domain.KindNotFound && de.Kind != domain.KindPreconditionFailed && !errors.Is(got, tt.err) {
				t.Fatal("cause lost")
			}
		})
	}
	if MapError(nil, OpRead, "x") != nil {
		t.Fatal("nil not nil")
	}
}

func TestRetryable(t *testing.T) {
	if !retryable(pgErr("40001")) || !retryable(pgErr("40P01")) || retryable(pgErr("23505")) || retryable(errors.New("x")) {
		t.Fatal("retryable wrong")
	}
}
