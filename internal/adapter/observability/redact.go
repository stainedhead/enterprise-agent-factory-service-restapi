package observability

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted replaces a removed value.
const Redacted = "[redacted]"

// secretKeys are attribute names whose value is never logged, compared case-insensitively with
// '-' and '_' ignored.
var secretKeys = map[string]bool{
	"authorization": true, "proxyauthorization": true, "xafsgatewaytoken": true, "token": true,
	"accesstoken": true, "idtoken": true, "refreshtoken": true, "bearer": true, "apikey": true,
	"secretkey": true, "secret": true, "secretvalue": true, "password": true, "passwd": true,
	"clientsecret": true, "cookie": true, "setcookie": true, "awssecretaccesskey": true, "sessiontoken": true,
}

// Value patterns scrubbed from every string the logger sees.
var scrubbers = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=\-]{6,}`), "$1 " + Redacted},
	{regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|access[_-]?key|authorization)(\s*[=:]\s*)[^\s,;&"']+`), "${1}${2}" + Redacted},
	{regexp.MustCompile(`\bafsk_[A-Za-z0-9_\-]{4,}`), "afsk_" + Redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}(?:\.[A-Za-z0-9_\-]*)?`), Redacted},
	{regexp.MustCompile(`(?i)(x-afs-gateway-token\s*[:=]\s*)\S+`), "${1}" + Redacted},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), Redacted},
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), Redacted},
}

// Scrub removes bearer tokens, afsk_ keys, JWT-shaped strings, gateway tokens, AWS access key ids and
// private keys from s.
func Scrub(s string) string {
	for _, sc := range scrubbers {
		s = sc.re.ReplaceAllString(s, sc.repl)
	}
	return s
}

func secretKey(k string) bool {
	k = strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(k))
	return secretKeys[k]
}

// RedactingHandler wraps a slog.Handler: it replaces the value of secret-named attributes, scrubs
// secret-shaped substrings from messages, strings, errors and stringified values, and adds the
// request id and trace ids from the context.
type RedactingHandler struct {
	inner     slog.Handler
	requestID func(context.Context) string
	traceIDs  func(context.Context) (trace, span string)
}

// NewRedactingHandler wraps inner. requestID may be nil.
func NewRedactingHandler(inner slog.Handler, requestID func(context.Context) string) *RedactingHandler {
	return &RedactingHandler{inner: inner, requestID: requestID, traceIDs: spanIDs}
}

// Enabled implements slog.Handler.
func (h *RedactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Scrub(r.Message), r.PC)
	if h.requestID != nil {
		if id := h.requestID(ctx); id != "" {
			out.AddAttrs(slog.String("requestId", Scrub(id)))
		}
	}
	if h.traceIDs != nil {
		if tr, sp := h.traceIDs(ctx); tr != "" {
			out.AddAttrs(slog.String("traceId", tr), slog.String("spanId", sp))
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return &RedactingHandler{inner: h.inner.WithAttrs(red), requestID: h.requestID, traceIDs: h.traceIDs}
}

// WithGroup implements slog.Handler.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: h.inner.WithGroup(name), requestID: h.requestID, traceIDs: h.traceIDs}
}

func redactAttr(a slog.Attr) slog.Attr {
	if secretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Scrub(v.String()))
	case slog.KindGroup:
		g := v.Group()
		out := make([]any, len(g))
		for i, ga := range g {
			out[i] = redactAttr(ga)
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		return slog.Attr{Key: a.Key, Value: redactAny(v.Any())}
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}

// redactAny handles errors, header-like maps and everything else by text: a value that contains
// nothing secret-shaped is passed through unchanged.
func redactAny(x any) slog.Value {
	switch t := x.(type) {
	case error:
		if s := t.Error(); Scrub(s) != s {
			return slog.StringValue(Scrub(s))
		}
		return slog.AnyValue(x)
	case map[string][]string: // http.Header and friends
		m := make(map[string]any, len(t))
		for k, vs := range t {
			if secretKey(k) {
				m[k] = Redacted
				continue
			}
			scrubbed := make([]string, len(vs))
			for i, s := range vs {
				scrubbed[i] = Scrub(s)
			}
			m[k] = scrubbed
		}
		return slog.AnyValue(m)
	case map[string]string:
		m := make(map[string]string, len(t))
		for k, s := range t {
			if secretKey(k) {
				m[k] = Redacted
			} else {
				m[k] = Scrub(s)
			}
		}
		return slog.AnyValue(m)
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, s := range t {
			if secretKey(k) {
				m[k] = Redacted
				continue
			}
			if rs, ok := s.(string); ok {
				m[k] = Scrub(rs)
				continue
			}
			m[k] = s
		}
		return slog.AnyValue(m)
	case fmt.Stringer:
		if s := t.String(); Scrub(s) != s {
			return slog.StringValue(Scrub(s))
		}
	}
	if s := fmt.Sprintf("%+v", x); Scrub(s) != s {
		return slog.StringValue(Scrub(s))
	}
	return slog.AnyValue(x)
}
