package collab

import (
	"regexp"
	"sort"
)

// Pattern types of the credential scanner (API contract 4.4c). The matched text is never stored: a Match
// holds the type and a count.
const (
	PatternAWSAccessKey      = "aws_access_key"
	PatternGitHubToken       = "github_token"
	PatternSlackToken        = "slack_token"
	PatternPrivateKey        = "private_key"
	PatternJWT               = "jwt"
	PatternOktaClientSecret  = "okta_client_secret"  //nolint:gosec // a pattern name, not a credential
	PatternEntraClientSecret = "entra_client_secret" //nolint:gosec // a pattern name, not a credential
	PatternGenericAssignment = "generic_assignment"
)

// PatternTypes lists the eight types in a fixed order.
func PatternTypes() []string {
	return []string{PatternAWSAccessKey, PatternGitHubToken, PatternSlackToken, PatternPrivateKey, PatternJWT,
		PatternOktaClientSecret, PatternEntraClientSecret, PatternGenericAssignment}
}

var patterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{PatternAWSAccessKey, regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{PatternGitHubToken, regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{50,255})\b`)},
	{PatternSlackToken, regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{PatternPrivateKey, regexp.MustCompile(`-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----`)},
	{PatternJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	{PatternOktaClientSecret, regexp.MustCompile(`(?i)okta[\w.-]{0,24}(?:client[_-]?secret|secret)["']?\s*[:=]\s*["']?[A-Za-z0-9_-]{32,}`)},
	{PatternEntraClientSecret, regexp.MustCompile(`\b[A-Za-z0-9_~.-]{3}8Q~[A-Za-z0-9_~.-]{30,}\b`)},
	{PatternGenericAssignment, regexp.MustCompile(`(?i)\b(?:password|passwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token)["']?\s*[:=]\s*["']?[A-Za-z0-9/+_=.-]{12,}`)},
}

// Match is one pattern type found and how many times.
type Match struct {
	Pattern string
	Count   int
}

// Scan returns the pattern types found in b, in PatternTypes order. It keeps no text.
func Scan(b []byte) []Match {
	var out []Match
	for _, p := range patterns {
		if n := len(p.re.FindAllIndex(b, -1)); n > 0 {
			out = append(out, Match{Pattern: p.name, Count: n})
		}
	}
	return out
}

// Window and overlap of the streaming scanner (EDGE-6): memory stays bounded and a match that spans two
// chunks is still found.
const (
	WindowBytes  = 64 * 1024
	OverlapBytes = 256
)

// Stream scans a body that arrives in chunks. Write is called for every chunk; Matches returns the
// totals. At most WindowBytes plus OverlapBytes are held at once.
type Stream struct {
	buf    []byte
	counts map[string]int
}

// NewStream returns an empty stream scanner.
func NewStream() *Stream { return &Stream{counts: map[string]int{}} }

// Write adds a chunk. It scans whenever a window is full.
func (s *Stream) Write(p []byte) {
	s.buf = append(s.buf, p...)
	for len(s.buf) >= WindowBytes+OverlapBytes {
		s.scanWindow(s.buf[:WindowBytes+OverlapBytes], WindowBytes)
		s.buf = append(s.buf[:0], s.buf[WindowBytes:]...)
	}
}

// scanWindow scans w and counts the matches that start before limit, so the overlap is counted by the
// next window only.
func (s *Stream) scanWindow(w []byte, limit int) {
	for _, p := range patterns {
		for _, loc := range p.re.FindAllIndex(w, -1) {
			if loc[0] < limit {
				s.counts[p.name]++
			}
		}
	}
}

// Finish scans what is left and returns the totals in PatternTypes order. The stream is spent.
func (s *Stream) Finish() []Match {
	s.scanWindow(s.buf, len(s.buf)+1)
	s.buf = nil
	var out []Match
	names := make([]string, 0, len(s.counts))
	for n := range s.counts {
		names = append(names, n)
	}
	order := map[string]int{}
	for i, n := range PatternTypes() {
		order[n] = i
	}
	sort.Slice(names, func(i, j int) bool { return order[names[i]] < order[names[j]] })
	for _, n := range names {
		out = append(out, Match{Pattern: n, Count: s.counts[n]})
	}
	return out
}

// TextContentType reports whether a body of this media type is scanned: JSON, text/*, form data and
// multipart (whose text parts the caller scans). Binary types are skipped.
func TextContentType(mediaType string) bool {
	switch {
	case mediaType == "":
		return true
	case len(mediaType) >= 5 && mediaType[:5] == "text/":
		return true
	}
	switch mediaType {
	case "application/json", "application/x-www-form-urlencoded", "multipart/form-data", "application/xml", "application/yaml", "application/x-yaml":
		return true
	}
	return len(mediaType) > 5 && (mediaType[len(mediaType)-5:] == "+json" || mediaType[len(mediaType)-4:] == "+xml")
}
