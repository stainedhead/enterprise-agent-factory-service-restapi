package httpapi

import (
	"net/http"
	"strconv"
	"time"
)

// The 1.1.0 release date and the planned 2.0.0 date: the defaults of the Deprecation and Sunset headers
// (API contract §4.4d, CD-29). The sunset is 12 months after the release; both are overridable by the
// Helm values api.deprecation.since and api.deprecation.sunset.
var (
	ReleaseDate1_1_0 = time.Date(2026, time.November, 23, 0, 0, 0, 0, time.UTC)
	DefaultSunset    = ReleaseDate1_1_0.AddDate(1, 0, 0)
)

// DeprecationPolicy says when the deprecated 1.0.1 surface was deprecated and when it goes away.
type DeprecationPolicy struct {
	Since  time.Time
	Sunset time.Time
}

// DefaultDeprecation is the policy handlers apply. cmd/afs-api replaces it from configuration at start,
// before the server accepts a request.
var DefaultDeprecation = DeprecationPolicy{Since: ReleaseDate1_1_0, Sunset: DefaultSunset}

// Apply sets Deprecation (an RFC 9745 structured date: "@" and the Unix time of the 1.1.0 release),
// Sunset (an HTTP-date) and Link with the successor path. Call it before the body is written.
func (p DeprecationPolicy) Apply(w http.ResponseWriter, successor string) {
	h := w.Header()
	h.Set("Deprecation", "@"+strconv.FormatInt(p.Since.Unix(), 10))
	h.Set("Sunset", p.Sunset.UTC().Format(http.TimeFormat))
	if successor != "" {
		h.Add("Link", "<"+successor+">; rel=\"successor-version\"")
	}
}
