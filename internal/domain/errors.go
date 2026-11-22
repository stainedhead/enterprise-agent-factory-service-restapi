package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorKind classifies a domain error. The HTTP layer maps each kind to one API contract §6 code.
type ErrorKind string

// Error kinds. BudgetExceeded and UpstreamError exist only in afs-gateway.
const (
	KindValidation           ErrorKind = "ValidationError"
	KindUnauthenticated      ErrorKind = "Unauthenticated"
	KindAccessDenied         ErrorKind = "AccessDenied"
	KindNotFound             ErrorKind = "NotFound"
	KindConflict             ErrorKind = "Conflict"
	KindPreconditionFailed   ErrorKind = "PreconditionFailed"
	KindPreconditionRequired ErrorKind = "PreconditionRequired"
	KindThrottled            ErrorKind = "Throttled"
	KindUnavailable          ErrorKind = "Unavailable"
	KindInternal             ErrorKind = "InternalError"
	KindBudgetExceeded       ErrorKind = "BudgetExceeded"
	KindUpstream             ErrorKind = "UpstreamError"
)

// Conflict reasons named by the API contract.
const (
	ReasonIdempotencyMismatch   = "idempotency_mismatch"
	ReasonIdempotencyInProgress = "idempotency_in_progress"
	ReasonTeamSuspended         = "team_suspended"
	ReasonNotImplemented        = "not_implemented"
)

// FieldError names one invalid request field.
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Error is the domain error type. Message is safe to show to people and never holds a secret.
type Error struct {
	Kind    ErrorKind
	Message string
	// Fields lists invalid fields (ValidationError).
	Fields []FieldError
	// Reason is a machine-readable reason (Conflict, InternalError).
	Reason string
	// RequiredPermission is set on AccessDenied.
	RequiredPermission string
	// Details holds further machine-readable details, such as missingTags or the dependents of a
	// Conflict. Values must be JSON-encodable and never hold a secret.
	Details map[string]any
	// RetryAfterSeconds asks the client to wait (Throttled, idempotency in progress).
	RetryAfterSeconds int
	// Cause is the wrapped error, never shown to clients.
	Cause error
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Kind))
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Cause != nil {
		fmt.Fprintf(&b, " (%v)", e.Cause)
	}
	return b.String()
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Cause }

// Is matches another *Error by kind, so errors.Is(err, &Error{Kind: KindNotFound}) works.
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Kind == e.Kind && (t.Reason == "" || t.Reason == e.Reason)
	}
	return false
}

// KindOf returns the kind of err, KindInternal for an error that is not a domain error, and "" for nil.
func KindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// Validation returns a ValidationError naming the invalid fields.
func Validation(fields ...FieldError) *Error {
	msg := "invalid request"
	if len(fields) > 0 {
		parts := make([]string, 0, len(fields))
		for _, f := range fields {
			parts = append(parts, f.Field+": "+f.Reason)
		}
		msg = strings.Join(parts, "; ")
	}
	return &Error{Kind: KindValidation, Message: msg, Fields: fields}
}

// NotFound returns a NotFound error for a resource kind. It never says whether the resource exists in
// another team.
func NotFound(resource string) *Error {
	return &Error{Kind: KindNotFound, Message: resource + " not found"}
}

// Conflict returns a Conflict error with a machine-readable reason and optional details.
func Conflict(reason, message string, details map[string]any) *Error {
	return &Error{Kind: KindConflict, Message: message, Reason: reason, Details: details}
}

// PreconditionFailed reports an If-Match that does not match the current version.
func PreconditionFailed() *Error {
	return &Error{Kind: KindPreconditionFailed, Message: "If-Match does not match the current version"}
}

// PreconditionRequired reports a missing If-Match on an update of an existing resource.
func PreconditionRequired() *Error {
	return &Error{Kind: KindPreconditionRequired, Message: "If-Match is required"}
}

// AccessDenied reports a valid caller without the permission perm.
func AccessDenied(perm string) *Error {
	return &Error{Kind: KindAccessDenied, Message: "requires permission " + perm, RequiredPermission: perm}
}

// Unauthenticated reports a missing, expired or unknown credential.
func Unauthenticated() *Error {
	return &Error{Kind: KindUnauthenticated, Message: "missing, expired or unknown credential"}
}

// Unavailable reports a dependency that is down; cause is kept for logs only.
func Unavailable(dependency string, cause error) *Error {
	return &Error{Kind: KindUnavailable, Message: dependency + " is unavailable", Cause: cause}
}

// Internal wraps an unexpected error; cause is kept for logs only.
func Internal(cause error) *Error {
	return &Error{Kind: KindInternal, Message: "internal error", Cause: cause}
}
