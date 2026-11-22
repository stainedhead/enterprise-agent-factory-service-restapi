package gatewayhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// problem is the error body of gateway contract §7: {code, message, requestId, details}. The Bedrock
// SDKs read "message" and the x-amzn-ErrorType header; the Anthropic SDKs read "type" and "error", so the
// Anthropic path carries those as well.
type problem struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"requestId"`
	Details   map[string]any `json:"details,omitempty"`
	// Anthropic shape, only on /anthropic paths.
	Type  string         `json:"type,omitempty"`
	Error *anthropicInfo `json:"error,omitempty"`
}

type anthropicInfo struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

// statusOf maps an error kind to the HTTP status of gateway contract §7.
func statusOf(err error) int {
	switch domain.KindOf(err) {
	case domain.KindValidation:
		return http.StatusBadRequest
	case domain.KindUnauthenticated:
		return http.StatusUnauthorized
	case domain.KindBudgetExceeded:
		return http.StatusPaymentRequired
	case domain.KindAccessDenied:
		return http.StatusForbidden
	case domain.KindNotFound:
		return http.StatusNotFound
	case domain.KindThrottled:
		return http.StatusTooManyRequests
	case domain.KindUpstream:
		return http.StatusBadGateway
	case domain.KindUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// bedrockErrorType is the x-amzn-ErrorType the AWS SDKs turn into their typed exceptions, so a refusal
// from the gateway looks like what the SDK already knows how to handle.
func bedrockErrorType(kind domain.ErrorKind) string {
	switch kind {
	case domain.KindValidation:
		return "ValidationException"
	case domain.KindUnauthenticated:
		return "UnrecognizedClientException"
	case domain.KindAccessDenied, domain.KindBudgetExceeded:
		return "AccessDeniedException"
	case domain.KindNotFound:
		return "ResourceNotFoundException"
	case domain.KindThrottled:
		return "ThrottlingException"
	case domain.KindUnavailable:
		return "ServiceUnavailableException"
	}
	return "InternalServerException"
}

// anthropicErrorType is the Anthropic error.type of a kind.
func anthropicErrorType(kind domain.ErrorKind) string {
	switch kind {
	case domain.KindValidation:
		return "invalid_request_error"
	case domain.KindUnauthenticated:
		return "authentication_error"
	case domain.KindAccessDenied, domain.KindBudgetExceeded:
		return "permission_error"
	case domain.KindNotFound:
		return "not_found_error"
	case domain.KindThrottled:
		return "rate_limit_error"
	case domain.KindUnavailable:
		return "overloaded_error"
	}
	return "api_error"
}

// toProblem turns an error into the body to send. The message of a non-domain error is never shown.
func toProblem(err error, requestID string, anthropic bool) (int, problem) {
	var de *domain.Error
	if !errors.As(err, &de) {
		de = &domain.Error{Kind: domain.KindInternal, Message: "internal error"}
	}
	p := problem{Code: string(de.Kind), Message: de.Message, RequestID: requestID, Details: de.Details}
	if p.Message == "" {
		p.Message = string(de.Kind)
	}
	if de.Kind == domain.KindAccessDenied && de.Reason != "" {
		if p.Details == nil {
			p.Details = map[string]any{}
		}
		p.Details["reason"] = de.Reason
	}
	if anthropic {
		p.Type = "error"
		p.Error = &anthropicInfo{Type: anthropicErrorType(de.Kind), Message: p.Message, Code: p.Code}
	}
	return statusOf(de), p
}

// writeProblem sends err as the contract's error body. status overrides the mapped status when not zero
// (413 is a ValidationError with another status).
func (rw *respWriter) writeProblem(err error, status int, anthropic bool) {
	st, p := toProblem(err, rw.requestID, anthropic)
	if status != 0 {
		st = status
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("X-Amzn-Errortype", bedrockErrorType(domain.KindOf(err)))
	var de *domain.Error
	if errors.As(err, &de) {
		switch {
		case de.RetryAfterSeconds > 0:
			rw.Header().Set("Retry-After", strconv.Itoa(de.RetryAfterSeconds))
		case de.Kind == domain.KindUnavailable || de.Kind == domain.KindUpstream:
			rw.Header().Set("Retry-After", "1")
		}
	}
	rw.WriteHeader(st)
	_ = json.NewEncoder(rw).Encode(p)
}

// clientGone reports that the error is the client's own cancellation, which needs no response.
func clientGone(err error) bool {
	return errors.Is(err, context.Canceled)
}

// exceptionType is the Bedrock event stream exception-type of a kind, for errors after the first event.
func exceptionType(kind domain.ErrorKind) string {
	switch kind {
	case domain.KindValidation:
		return "validationException"
	case domain.KindThrottled:
		return "throttlingException"
	case domain.KindUnavailable:
		return "serviceUnavailableException"
	}
	return "internalServerException"
}

// kindOfException maps an upstream exception-type back to a gateway error kind.
func kindOfException(t string) domain.ErrorKind {
	switch t {
	case "throttlingException":
		return domain.KindThrottled
	case "validationException":
		return domain.KindValidation
	}
	// Everything else Bedrock reports, a server error included, is an UpstreamError (gateway contract §7).
	return domain.KindUpstream
}
