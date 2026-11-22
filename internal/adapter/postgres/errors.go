package postgres

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Op says what a statement was doing, because database contract §6 maps 23503 differently on a
// write (unknown reference) and on a delete (dependents exist).
type Op int

// Statement kinds.
const (
	OpWrite Op = iota
	OpDelete
	OpRead
)

// SQLSTATEs named by database contract §5 and §6.
const (
	codeSerializationFailure = "40001"
	codeDeadlock             = "40P01"
	codeCheckViolation       = "23514"
	codeNotNull              = "23502"
	codeUnique               = "23505"
	codeForeignKey           = "23503"
	codeNotInPrerequisite    = "55000"
	codeLockNotAvailable     = "55P03"
	codeNoDataFound          = "P0002"
	codeInsufficientPriv     = "42501"
	codeInvalidParameter     = "22023"
	codeQueryCanceled        = "57014"
	codeTooManyConnections   = "53300"
)

// retryable reports a serialization failure or deadlock (database contract §5.6).
func retryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == codeSerializationFailure || pe.Code == codeDeadlock)
}

// MapError turns a pgx error into a domain error per database contract §6. A domain error passes
// through unchanged; nil stays nil. pgx.ErrNoRows becomes NotFound for resource.
func MapError(err error, op Op, resource string) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound(resource)
	}
	if unavailable(err) {
		e := domain.Unavailable("database", err)
		e.RetryAfterSeconds = 1
		return e
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return domain.Internal(err)
	}
	switch {
	case pe.Code == codeCheckViolation || pe.Code == codeNotNull:
		return validation(pe, err)
	case pe.Code == codeInvalidParameter:
		slog.Warn("postgres: invalid parameter value", "sqlstate", pe.Code, "message", pe.Message, "routine", pe.Routine)
		return validation(pe, err)
	case strings.HasPrefix(pe.Code, "22"):
		return validation(pe, err)
	case pe.Code == codeUnique:
		if reason, ok := uniqueReasons[pe.ConstraintName]; ok {
			return withCause(domain.Conflict(reason, pe.Message, constraintDetails(pe)), err)
		}
		return withCause(domain.Conflict("duplicate", pe.Message, constraintDetails(pe)), err)
	case pe.Code == codeForeignKey && op == OpDelete:
		return withCause(domain.Conflict("dependents_exist", pe.Message, constraintDetails(pe)), err)
	case pe.Code == codeForeignKey:
		return validation(pe, err)
	case pe.Code == codeNotInPrerequisite:
		return withCause(domain.Conflict("invalid_state", pe.Message, nil), err)
	case pe.Code == codeInsufficientPriv:
		e := domain.AccessDenied("")
		e.Message = pe.Message
		e.Cause = err
		return e
	case pe.Code == codeNoDataFound:
		slog.Warn("postgres: no data found", "message", pe.Message, "routine", pe.Routine)
		return domain.Internal(err)
	default:
		return domain.Internal(err)
	}
}

// uniqueReasons names the unique violations that carry a contract reason (Paperclip change): a second
// live collaboration agent, and a team slug whose collaboration hostname is still held (RD-22).
var uniqueReasons = map[string]string{
	"agents_team_collaboration_uq": domain.ReasonCollaborationAgentExists,
	"collab_hostname_held":         domain.ReasonHostnameHeld,
}

// constraintFields maps the Paperclip check constraints to the request field they refuse.
var constraintFields = map[string]string{
	"agents_collaboration_template":      "template",
	"agents_collaboration_capability":    "template",
	"agents_worker_capability":           "template",
	"agents_collaboration_identity":      "iamIdentity",
	"agents_collaboration_snapshots":     "volume.snapshotSchedule",
	"agents_role_immutable":              "role",
	"secrets_reserved_name":              "name",
	"secrets_managed_by_name":            "name",
	"secrets_paperclip_scope":            "name",
	"secrets_paperclip_prefix":           "name",
	"secrets_registered_paperclip_check": "name",
}

func validation(pe *pgconn.PgError, cause error) *domain.Error {
	field := pe.ColumnName
	if field == "" {
		field = pe.ConstraintName
	}
	if f, ok := constraintFields[pe.ConstraintName]; ok {
		field = f
	}
	e := domain.Validation(domain.FieldError{Field: field, Reason: pe.Message})
	e.Message = pe.Message
	e.Cause = cause
	return e
}

func constraintDetails(pe *pgconn.PgError) map[string]any {
	if pe.ConstraintName == "" {
		return nil
	}
	return map[string]any{"constraint": pe.ConstraintName}
}

func withCause(e *domain.Error, cause error) *domain.Error {
	e.Cause = cause
	return e
}

// unavailable reports connection failures, timeouts and server shutdowns: the request may succeed
// later, so the API answers 503.
func unavailable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return strings.HasPrefix(pe.Code, "08") || strings.HasPrefix(pe.Code, "57P") ||
			pe.Code == codeQueryCanceled || pe.Code == codeTooManyConnections || pe.Code == codeLockNotAvailable
	}
	return false
}
