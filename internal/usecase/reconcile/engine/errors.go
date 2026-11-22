package engine

import (
	"errors"
	"fmt"
)

// ConditionError attributes a failure to one condition type and carries the AWS or Kubernetes error
// code that becomes the condition's reason.
type ConditionError struct {
	// Type is the condition that failed, for example IamReady. Empty means Ready.
	Type string
	// Code is the AWS or Kubernetes error code (AccessDenied, Forbidden, ...). Empty derives it from
	// Err with CodeOf.
	Code string
	Err  error
}

// Error implements error.
func (e *ConditionError) Error() string {
	if e.Type == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %v", e.Type, e.Err)
}

// Unwrap returns the cause.
func (e *ConditionError) Unwrap() error { return e.Err }

// WithCondition wraps err as a failure of condition typ.
func WithCondition(typ string, err error) error {
	if err == nil {
		return nil
	}
	return &ConditionError{Type: typ, Err: err}
}

// coder is implemented by errors that know their AWS or Kubernetes error code (smithy.APIError has
// ErrorCode; adapters wrap Kubernetes status reasons in CodedError).
type coder interface{ ErrorCode() string }

// CodedError is an error with a machine-readable code.
type CodedError struct {
	Code string
	Err  error
}

// Error implements error.
func (e *CodedError) Error() string { return e.Err.Error() }

// Unwrap returns the cause.
func (e *CodedError) Unwrap() error { return e.Err }

// ErrorCode returns the code.
func (e *CodedError) ErrorCode() string { return e.Code }

// CodeOf returns the error code carried by err (or a wrapped error), or "" when there is none.
func CodeOf(err error) string {
	var ce *ConditionError
	if errors.As(err, &ce) && ce.Code != "" {
		return ce.Code
	}
	var c coder
	if errors.As(err, &c) {
		return c.ErrorCode()
	}
	return ""
}

// ErrTerminal marks a failure retrying cannot fix without a change of the desired state (a refused
// configuration, a missing prerequisite). The resource is reported Failed; it is still retried, at
// the longest backoff, in case the outside world changed.
var ErrTerminal = errors.New("terminal reconcile error")

// Terminal wraps err as terminal.
func Terminal(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrTerminal, err)
}

// reasonOf derives the condition type and reason of a failure.
func reasonOf(err error) (condType, reason string) {
	condType = "Ready"
	var ce *ConditionError
	if errors.As(err, &ce) && ce.Type != "" {
		condType = ce.Type
	}
	reason = CodeOf(err)
	if reason == "" {
		reason = "ReconcileError"
	}
	return condType, reason
}
