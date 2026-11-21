package domain

import (
	"strconv"
	"strings"
)

// Version is the optimistic-concurrency version of a mutable resource; ETag is "<version>".
type Version int64

// ETag returns the quoted entity tag for v.
func (v Version) ETag() string { return `"` + strconv.FormatInt(int64(v), 10) + `"` }

// Generation counts desired-state changes; status.observedGeneration equals it once applied.
type Generation int64

// ParseIfMatch parses an If-Match header value. Accepted forms: "7", 7 and W/"7". An empty header
// is PreconditionRequired; anything else that is not a single positive version is a ValidationError.
func ParseIfMatch(h string) (Version, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, PreconditionRequired()
	}
	h = strings.TrimPrefix(h, "W/")
	h = strings.TrimSuffix(strings.TrimPrefix(h, `"`), `"`)
	n, err := strconv.ParseInt(h, 10, 64)
	if err != nil || n < 1 {
		return 0, Validation(FieldError{Field: "If-Match", Reason: `must be a version, for example "3"`})
	}
	return Version(n), nil
}

// CheckVersion returns PreconditionFailed when want differs from current.
func CheckVersion(current, want Version) error {
	if current != want {
		return PreconditionFailed()
	}
	return nil
}
