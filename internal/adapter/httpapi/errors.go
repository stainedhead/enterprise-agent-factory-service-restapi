package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// statusOf maps API contract §6 codes to HTTP statuses.
var statusOf = map[domain.ErrorKind]int{
	domain.KindValidation:           http.StatusBadRequest,
	domain.KindUnauthenticated:      http.StatusUnauthorized,
	domain.KindAccessDenied:         http.StatusForbidden,
	domain.KindNotFound:             http.StatusNotFound,
	domain.KindConflict:             http.StatusConflict,
	domain.KindPreconditionFailed:   http.StatusPreconditionFailed,
	domain.KindPreconditionRequired: http.StatusPreconditionRequired,
	domain.KindThrottled:            http.StatusTooManyRequests,
	domain.KindInternal:             http.StatusInternalServerError,
	domain.KindUnavailable:          http.StatusServiceUnavailable,
}

// errorBody is the contract §6 envelope {code, message, requestId, details}.
type errorBody struct {
	Code      domain.ErrorKind `json:"code"`
	Message   string           `json:"message"`
	RequestID string           `json:"requestId"`
	Details   map[string]any   `json:"details"`
}

// toDomain returns err as a domain error; anything else is an InternalError.
func toDomain(err error) *domain.Error {
	var de *domain.Error
	if errors.As(err, &de) {
		return de
	}
	return domain.Internal(err)
}

// WriteError writes err as the contract error envelope. Internal and Unavailable causes are logged
// with the request id and never shown to the client.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	writeErrorStatus(w, r, err, 0)
}

func writeErrorStatus(w http.ResponseWriter, r *http.Request, err error, override int) {
	de := toDomain(err)
	status, ok := statusOf[de.Kind]
	if !ok {
		// BudgetExceeded and UpstreamError are gateway-only; afs-api never answers them.
		de = domain.Internal(err)
		status = http.StatusInternalServerError
	}
	if override != 0 {
		status = override
	}
	rid := RequestIDFrom(r.Context())
	msg := de.Message
	if status >= 500 && de.Reason != domain.ReasonNotImplemented {
		slog.ErrorContext(r.Context(), "request failed", "requestId", rid, "code", de.Kind, "err", de.Error())
		if de.Kind == domain.KindInternal {
			msg = "internal error"
		}
	}
	details := map[string]any{}
	for k, v := range de.Details {
		details[k] = v
	}
	if len(de.Fields) > 0 {
		details["fields"] = de.Fields
	}
	if de.RequiredPermission != "" {
		details["requiredPermission"] = de.RequiredPermission
	}
	if de.Reason != "" {
		details["reason"] = de.Reason
	}
	if de.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(de.RetryAfterSeconds))
	}
	writeJSON(w, status, errorBody{Code: de.Kind, Message: msg, RequestID: rid, Details: details})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteJSON writes v with status as application/json.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }
