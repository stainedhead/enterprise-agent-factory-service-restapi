package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Helpers shared by the handlers of M1-B, M1-C and M1-E (teams, members, API keys, service
// administration I). Names carry the prefix w2 so they cannot collide with another stream's.

// w2SetArea replaces the registrar of the named area in Areas.
func w2SetArea(name string, r Registrar) {
	for i := range Areas {
		if Areas[i].Name == name {
			Areas[i].Register = r
			return
		}
	}
	panic("httpapi: unknown area " + name)
}

// w2Caller builds the use case caller from the authenticated request. The source address is the
// connection peer; a trusted proxy header is not honored here.
func w2Caller(r *http.Request) (acting.Caller, error) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		return acting.Caller{}, domain.Unauthenticated()
	}
	ip := r.RemoteAddr
	if h, _, err := net.SplitHostPort(ip); err == nil {
		ip = h
	}
	ua := r.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	return acting.Caller{Principal: p, RequestID: RequestIDFrom(r.Context()), SourceIP: ip, UserAgent: ua}, nil
}

// w2IfMatch parses a required If-Match: absent is 428, malformed is 400.
func w2IfMatch(r *http.Request) (domain.Version, error) {
	return domain.ParseIfMatch(r.Header.Get("If-Match"))
}

// w2IfMatchOptional parses If-Match for a PUT upsert (CD-2): absent is nil.
func w2IfMatchOptional(r *http.Request) (*domain.Version, error) {
	if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
		return nil, nil
	}
	v, err := domain.ParseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// w2Body reads the body once and leaves it readable for Decode.
func w2Body(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, bad("body", "request body is larger than the limit")
		}
		return nil, bad("body", "unreadable request body")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return raw, nil
}

// w2Idem builds the idempotency claim of a create when the request has an Idempotency-Key; nil
// otherwise. render produces the status and body that are stored and replayed.
func w2Idem(r *http.Request, op, teamID string, body []byte, render w2Renderer) (*acting.Idem, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, nil
	}
	if len(key) > 255 {
		return nil, bad("Idempotency-Key", "must be 1 to 255 printable characters")
	}
	for i := range len(key) {
		if key[i] < 0x21 || key[i] > 0x7e {
			return nil, bad("Idempotency-Key", "must be 1 to 255 printable characters")
		}
	}
	return &acting.Idem{Key: port.IdempotencyKey{Key: key, Operation: op, TeamID: teamID, RequestSHA256: sha256.Sum256(body)}, Render: render}, nil
}

// w2Replay writes a stored idempotent response.
func w2Replay(w http.ResponseWriter, st *port.StoredResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replayed", "true")
	var v struct {
		Version *int64 `json:"version"`
	}
	if json.Unmarshal(st.Body, &v) == nil && v.Version != nil {
		w.Header().Set("ETag", domain.Version(*v.Version).ETag())
	}
	w.WriteHeader(st.Status)
	_, _ = w.Write(st.Body)
}

// w2Renderer produces the status and body of a stored response.
type w2Renderer = func(any) (int, json.RawMessage, error)

// w2Render marshals v for storage with status.
func w2Render(status int, v any) (int, json.RawMessage, error) {
	b, err := json.Marshal(v)
	return status, b, err
}

func w2Time(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func w2TimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := w2Time(*t)
	return &s
}

func w2Etag(w http.ResponseWriter, v domain.Version) { w.Header().Set("ETag", v.ETag()) }

// w2Page parses maxResults and nextToken for the operation of the request.
func w2Page(pages PageTokens, r *http.Request, c acting.Caller) (acting.Page, error) {
	rt, _ := RouteFrom(r)
	p, err := pages.ParsePage(r, rt.OperationID, c.Principal.ID)
	return acting.Page{Limit: p.Limit, Cursor: p.Cursor}, err
}

// w2Next is the nextToken for cursor, "" on the last page.
func w2Next(pages PageTokens, r *http.Request, c acting.Caller, cursor string) string {
	rt, _ := RouteFrom(r)
	return pages.Encode(rt.OperationID, c.Principal.ID, cursor)
}

// w2Nullable decodes a JSON member that may be absent, null or a value.
type w2Nullable[T any] struct {
	Set   bool
	Null  bool
	Value T
}

// UnmarshalJSON implements json.Unmarshaler; it is only called when the member is present.
func (n *w2Nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(bytes.TrimSpace(b)) == "null" {
		n.Null = true
		return nil
	}
	return json.Unmarshal(b, &n.Value)
}

func w2ParseTime(field, s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, bad(field, "must be an RFC 3339 timestamp")
	}
	return t.UTC(), nil
}

func w2Int64(field, s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 0, bad(field, "must be a positive integer")
	}
	return n, nil
}

// w2ListBody is a list response: items and, when more follow, nextToken.
type w2ListBody[T any] struct {
	Items     []T    `json:"items"`
	NextToken string `json:"nextToken,omitempty"`
}
