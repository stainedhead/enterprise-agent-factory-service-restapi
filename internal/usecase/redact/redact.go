// Package redact is the credential redaction filter of RC-18 and RC-7 (RC-US-16.1, RC-US-16.2).
// Credential-like strings (passwords, bearer tokens, AWS access keys, private keys, JWTs, URL
// passwords) and the values of a team's bound secrets are replaced by
// [redacted:<first 8 hex of HMAC-SHA-256 under the team's key>]. The token is deterministic, so the
// same value gives the same token every time (and in every encoding it is found in), which lets an
// operator correlate lines without seeing the value; it reveals nothing without the key. The filter is
// idempotent: redacting redacted text changes nothing.
//
// It is pure Go on the standard library. The bound secret values live only in the Redactor in memory;
// nothing here stores, logs or returns one.
package redact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// MinKeyLen is the shortest per-team key accepted.
const MinKeyLen = 16

// MinSecretLen is the shortest bound secret value matched literally. A shorter value would corrupt
// ordinary text ("1", "true") far more often than it would catch a leak.
const MinSecretLen = 6

// Prefix starts every token.
const Prefix = "[redacted:"

var tokenRE = regexp.MustCompile(`^\[redacted:[0-9a-f]{8}\]$`)

// DeriveTeamKey derives a team's redaction key from a service master key, so no per-team key needs
// storing. master must be at least MinKeyLen bytes.
func DeriveTeamKey(master []byte, teamID string) ([]byte, error) {
	if len(master) < MinKeyLen {
		return nil, errors.New("redact: master key is too short")
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte("afs-redact-team-key:" + teamID))
	return m.Sum(nil), nil
}

// pattern is a value-shaped credential. group is the capture group holding the value to replace (0:
// the whole match).
type pattern struct {
	re    *regexp.Regexp
	group int
	// accept, when set, vets the value before it is replaced (keeps prose such as "bearer
	// authentication" intact).
	accept func(string) bool
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

const keyWords = `(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|credentials?|authorization|private[_-]?key)`

var patterns = []pattern{
	// -----BEGIN … PRIVATE KEY----- blocks, through the END line or to the end of the text.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), 0, nil},
	// JWTs.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), 0, nil},
	// AWS access key ids.
	{regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`), 0, nil},
	// Passwords in URLs: scheme://user:password@host.
	{regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]*:([^\s/@]+)@`), 1, nil},
	// key=value, key: value and JSON "key": "value" where the key names a credential. A key must end
	// with the word, so max_tokens and secretId are not credentials. An Authorization value may be
	// "Bearer <token>" or "Basic <token>".
	{regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*` + keyWords + `["']?\s*[=:]\s*(?:"([^"\n]*)"|'([^'\n]*)'|((?:(?:bearer|basic)\s+)?(?:\[redacted:[0-9a-f]{8}\]|[^\s,;&"'}\]]+)))`), -1, nil},
	// Bearer and Basic credentials on their own.
	{regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+([A-Za-z0-9._~+/=-]{8,})`), 1, hasDigit},
}

// Redactor replaces credentials with tokens of one team's key.
type Redactor struct {
	key     []byte
	secrets []literal
	skipped int
}

// literal is one spelling of a bound secret value and the token it gets.
type literal struct {
	text  string
	token string
}

// New returns a Redactor for a team. key is the team's key (at least MinKeyLen bytes); secrets are
// the values of the team's bound secrets, matched literally in their plain, JSON-escaped and
// URL-escaped spellings, longest first. Values shorter than MinSecretLen are not matched (Skipped
// counts them).
func New(key []byte, secrets ...[]byte) (*Redactor, error) {
	if len(key) < MinKeyLen {
		return nil, errors.New("redact: key is too short")
	}
	r := &Redactor{key: append([]byte(nil), key...)}
	seen := map[string]bool{}
	for _, v := range secrets {
		raw := string(v)
		if len(raw) < MinSecretLen {
			if len(raw) > 0 {
				r.skipped++
			}
			continue
		}
		tok := r.Token(raw)
		spellings := []string{raw}
		if j, err := json.Marshal(raw); err == nil {
			if esc := string(j[1 : len(j)-1]); esc != raw {
				spellings = append(spellings, esc)
			}
		}
		if esc := url.QueryEscape(raw); esc != raw {
			spellings = append(spellings, esc)
		}
		for _, s := range spellings {
			if !seen[s] {
				seen[s] = true
				r.secrets = append(r.secrets, literal{text: s, token: tok})
			}
		}
	}
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i].text) > len(r.secrets[j].text) })
	return r, nil
}

// Skipped is the number of bound secret values too short to match.
func (r *Redactor) Skipped() int { return r.skipped }

// Token is the replacement of value: [redacted:<first 8 hex of HMAC-SHA-256(key, value)>].
func (r *Redactor) Token(value string) string {
	m := hmac.New(sha256.New, r.key)
	m.Write([]byte(value))
	return Prefix + hex.EncodeToString(m.Sum(nil)[:4]) + "]"
}

// Redact returns s with bound secret values and credential-like strings replaced by tokens.
func (r *Redactor) Redact(s string) string {
	for _, l := range r.secrets {
		if strings.Contains(s, l.text) {
			s = strings.ReplaceAll(s, l.text, l.token)
		}
	}
	for _, p := range patterns {
		s = r.apply(p, s)
	}
	return s
}

// RedactBytes is Redact for a byte slice.
func (r *Redactor) RedactBytes(b []byte) []byte { return []byte(r.Redact(string(b))) }

func (r *Redactor) apply(p pattern, s string) string {
	idx := p.re.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		start, end := m[0], m[1]
		if p.group > 0 {
			start, end = m[2*p.group], m[2*p.group+1]
		} else if p.group < 0 { // the first participating group of the key=value pattern
			start, end = -1, -1
			for g := 1; 2*g+1 < len(m); g++ {
				if m[2*g] >= 0 {
					start, end = m[2*g], m[2*g+1]
					break
				}
			}
		}
		if start < 0 || start == end || tokenRE.MatchString(s[start:end]) || (p.accept != nil && !p.accept(s[start:end])) {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(r.Token(s[start:end]))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}
