package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// maxTokenLength bounds the credential read from a request: longer values are refused before any
// parsing or lookup.
const maxTokenLength = 16 << 10

// Credentials turns a bearer token into a principal (auth.Service). It returns domain.Unauthenticated
// for a token that is invalid, expired or unknown and domain.Unavailable for a dependency that is down.
type Credentials interface {
	Authenticate(ctx context.Context, token string) (*authz.Principal, error)
}

// Authn implements httpapi.Authenticator on bearer credentials and applies the request rate limits
// that depend on who is calling (RA-US-15.1): per principal once authenticated, per source address for
// requests that carry no valid credential. A refused request is answered with 429 instead of 401.
//
// An Authorization header is never logged or copied into an error.
type Authn struct {
	Auth Credentials
	// Limiter may be nil, which disables rate limiting.
	Limiter *Limiter
}

// Authenticate implements httpapi.Authenticator.
func (a *Authn) Authenticate(r *http.Request) (*authz.Principal, error) {
	token, err := Bearer(r)
	if err != nil || token == "" {
		if lerr := a.limitSource(r); lerr != nil {
			return nil, lerr
		}
		return nil, err
	}
	p, err := a.Auth.Authenticate(r.Context(), token)
	if err != nil {
		if domain.KindOf(err) == domain.KindUnauthenticated {
			if lerr := a.limitSource(r); lerr != nil {
				return nil, lerr
			}
		}
		return nil, err
	}
	if p == nil {
		return nil, domain.Unauthenticated()
	}
	if a.Limiter != nil {
		if ok, wait := a.Limiter.Allow("principal:" + p.ID); !ok {
			return nil, Throttled(wait)
		}
	}
	return p, nil
}

func (a *Authn) limitSource(r *http.Request) error {
	if a.Limiter == nil {
		return nil
	}
	if ok, wait := a.Limiter.Allow(sourceKey(r)); !ok {
		return Throttled(wait)
	}
	return nil
}

// Bearer extracts the token of an "Authorization: Bearer <token>" header. No header returns ("", nil),
// which the route authorization turns into 401; a header that is not a single well-formed bearer
// credential is Unauthenticated.
func Bearer(r *http.Request) (string, error) {
	vals := r.Header.Values("Authorization")
	switch len(vals) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", domain.Unauthenticated()
	}
	scheme, token, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", domain.Unauthenticated()
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenLength || strings.ContainsAny(token, " \t\r\n") {
		return "", domain.Unauthenticated()
	}
	return token, nil
}
