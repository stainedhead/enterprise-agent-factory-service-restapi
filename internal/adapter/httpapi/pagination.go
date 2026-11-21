package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Page sizes (API contract §1: maxResults 1–100, default 50).
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
	pageTokenTTL    = 24 * time.Hour
)

// PageTokens signs and verifies opaque nextToken values. A token is bound to the operation and
// the caller, and expires after 24 hours.
type PageTokens struct {
	key []byte
	now func() time.Time
}

// NewPageTokens returns PageTokens signing with key (at least 32 bytes).
func NewPageTokens(key []byte, now func() time.Time) PageTokens {
	if now == nil {
		now = time.Now
	}
	return PageTokens{key: key, now: now}
}

type tokenBody struct {
	Op     string `json:"o"`
	Caller string `json:"p"`
	Cursor string `json:"c"`
	Exp    int64  `json:"e"`
}

func (t PageTokens) mac(b []byte) []byte {
	m := hmac.New(sha256.New, t.key)
	m.Write(b)
	return m.Sum(nil)
}

// Encode returns the nextToken for cursor; "" for an empty cursor (last page).
func (t PageTokens) Encode(op, caller, cursor string) string {
	if cursor == "" {
		return ""
	}
	b, _ := json.Marshal(tokenBody{Op: op, Caller: caller, Cursor: cursor, Exp: t.now().Add(pageTokenTTL).Unix()})
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(t.mac(b))
}

// Decode verifies token and returns its cursor. A forged, foreign or expired token is a
// ValidationError on nextToken.
func (t PageTokens) Decode(op, caller, token string) (string, error) {
	invalid := bad("nextToken", "invalid or expired token")
	var body, sig string
	for i := range len(token) {
		if token[i] == '.' {
			body, sig = token[:i], token[i+1:]
			break
		}
	}
	b, err1 := base64.RawURLEncoding.DecodeString(body)
	s, err2 := base64.RawURLEncoding.DecodeString(sig)
	if body == "" || err1 != nil || err2 != nil || !hmac.Equal(s, t.mac(b)) {
		return "", invalid
	}
	var tb tokenBody
	if json.Unmarshal(b, &tb) != nil || tb.Op != op || tb.Caller != caller || t.now().Unix() > tb.Exp {
		return "", invalid
	}
	return tb.Cursor, nil
}

// Page is a parsed page request.
type Page struct {
	Limit  int
	Cursor string
}

// ParsePage reads maxResults and nextToken for op and caller.
func (t PageTokens) ParsePage(r *http.Request, op, caller string) (Page, error) {
	p := Page{Limit: DefaultPageSize}
	q := r.URL.Query()
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPageSize {
			return Page{}, bad("maxResults", "must be an integer from 1 to 100")
		}
		p.Limit = n
	}
	if v := q.Get("nextToken"); v != "" {
		c, err := t.Decode(op, caller, v)
		if err != nil {
			return Page{}, err
		}
		p.Cursor = c
	}
	return p, nil
}
