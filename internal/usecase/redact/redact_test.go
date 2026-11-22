package redact_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/redact"
)

var (
	keyA       = []byte("team-a-key-0123456789")
	keyB       = []byte("team-b-key-9876543210")
	tok        = regexp.MustCompile(`\[redacted:[0-9a-f]{8}\]`)
	knownHello = "9d1793a2"
)

func mustNew(t *testing.T, key []byte, secrets ...string) *redact.Redactor {
	t.Helper()
	var s [][]byte
	for _, v := range secrets {
		s = append(s, []byte(v))
	}
	r, err := redact.New(key, s...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// RC-US-16.2: strings that look like credentials are redacted.
func TestPatternsAreRedacted(t *testing.T) {
	r := mustNew(t, keyA)
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r"
	tests := []struct {
		name, in string
		gone     string // must not survive
		keep     string // must survive
	}{
		{"password kv", "login ok password=hunter2secret user=bob", "hunter2secret", "user=bob"},
		{"password json", `{"user":"bob","password":"p@ss w0rd!"}`, "p@ss w0rd!", `"user":"bob"`},
		{"password single quotes", `DB_PASSWORD: 'correct horse'`, "correct horse", "DB_PASSWORD:"},
		{"token env", "GITHUB_TOKEN=ghp_abcdefghijklmnop1234", "ghp_abcdefghijklmnop1234", "GITHUB_TOKEN="},
		{"api key", "api_key: sk-live-0123456789", "sk-live-0123456789", "api_key:"},
		{"authorization bearer", "Authorization: Bearer abcdefgh12345678", "abcdefgh12345678", "Authorization:"},
		{"bearer alone", "curl -H 'Bearer zyxwvutsr98765'", "zyxwvutsr98765", "curl"},
		{"basic", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA==", "Authorization:"},
		{"aws key id", "key AKIAIOSFODNN7EXAMPLE used", "AKIAIOSFODNN7EXAMPLE", "key"},
		{"aws temp key id", "ASIAIOSFODNN7EXAMPLE", "ASIAIOSFODNN7EXAMPLE", ""},
		{"aws secret", "aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "aws_secret_access_key="},
		{"jwt", "token was " + jwt + " ok", jwt, "ok"},
		{"url password", "dial postgres://afs:s3cr3tpw@db.example.com:5432/afs", "s3cr3tpw", "postgres://afs:"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY----- tail", "MIIEowIBAAKCAQEA", "tail"},
		{"private key unterminated", "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkq", "MIIEvQIBADANBgkq", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Redact(tc.in)
			if strings.Contains(got, tc.gone) || !tok.MatchString(got) {
				t.Fatalf("not redacted: %q", got)
			}
			if tc.keep != "" && !strings.Contains(got, tc.keep) {
				t.Fatalf("lost %q: %q", tc.keep, got)
			}
			if again := r.Redact(got); again != got {
				t.Fatalf("not idempotent: %q then %q", got, again)
			}
		})
	}
}

func TestOrdinaryTextIsUntouched(t *testing.T) {
	r := mustNew(t, keyA)
	for _, s := range []string{
		"request completed in 35ms",
		`{"max_tokens":100,"tokens_used":42,"secretId":"sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"}`,
		"password reset email sent",
		"token refresh scheduled",
		"user bob authenticated",
		"bearer authentication is required",
		"password:",
		"password=",
		"id=AKIA short",
		"https://example.com/path?x=1",
		"",
	} {
		if got := r.Redact(s); got != s {
			t.Errorf("%q became %q", s, got)
		}
	}
}

// RC-US-16.1: the value of a bound secret becomes a token, the same token each time, in every spelling.
func TestBoundSecretsAreRedactedDeterministically(t *testing.T) {
	const val = "s3cr3t/value+with\"chars"
	r := mustNew(t, keyA, val, "short", "another-bound-secret")
	line := "using " + val + " and again " + val + " and another-bound-secret end"
	got := r.Redact(line)
	if strings.Contains(got, val) || strings.Contains(got, "another-bound-secret") {
		t.Fatalf("leaked: %q", got)
	}
	toks := tok.FindAllString(got, -1)
	if len(toks) != 3 || toks[0] != toks[1] || toks[0] == toks[2] {
		t.Fatalf("tokens %v", toks)
	}
	if again := mustNew(t, keyA, val).Redact("x " + val); !strings.Contains(again, toks[0]) {
		t.Fatalf("same value, same key, different token: %q vs %v", again, toks)
	}
	if toks[0] != r.Token(val) {
		t.Fatal("Token disagrees with Redact")
	}
	// JSON-escaped and URL-escaped spellings carry the same token.
	for name, spelled := range map[string]string{"json": `{"msg":"v=s3cr3t/value+with\"chars"}`, "url": "?v=s3cr3t%2Fvalue%2Bwith%22chars"} {
		out := r.Redact(spelled)
		if strings.Contains(out, "s3cr3t") || !strings.Contains(out, toks[0]) {
			t.Errorf("%s spelling: %q", name, out)
		}
	}
	// A value too short to match literally is counted, not silently ignored.
	if r.Skipped() != 1 || r.Redact("short stays") != "short stays" {
		t.Fatalf("skipped=%d", r.Skipped())
	}
}

// The token depends on the team's key: another team's key gives another token, and the token is
// 8 hex characters of HMAC-SHA-256 (it cannot be reversed or compared across teams).
func TestTokenIsPerTeam(t *testing.T) {
	a, b := mustNew(t, keyA), mustNew(t, keyB)
	first := a.Token("value-123456")
	if first == b.Token("value-123456") || first != a.Token("value-123456") {
		t.Fatal("token not per-team and deterministic")
	}
	if !tok.MatchString(a.Token("x")) {
		t.Fatalf("shape: %s", a.Token("x"))
	}
	// Known answer: HMAC-SHA-256(key="team-a-key-0123456789", msg="hello") first 4 bytes.
	if got := a.Token("hello"); got != "[redacted:"+knownHello+"]" {
		t.Fatalf("Token(hello) = %s", got)
	}
}

// Longer bound secrets win over their own prefixes.
func TestLongestSecretFirst(t *testing.T) {
	r := mustNew(t, keyA, "abcdef", "abcdef-extended-value")
	got := r.Redact("v=abcdef-extended-value")
	if strings.Contains(got, "extended") || strings.Count(got, "[redacted:") != 1 {
		t.Fatalf("%q", got)
	}
}

func TestKeyChecks(t *testing.T) {
	if _, err := redact.New([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := redact.DeriveTeamKey([]byte("short"), "team_x"); err == nil {
		t.Fatal("short master accepted")
	}
	m := []byte("master-key-0123456789")
	a, _ := redact.DeriveTeamKey(m, "team_a")
	a2, _ := redact.DeriveTeamKey(m, "team_a")
	b, _ := redact.DeriveTeamKey(m, "team_b")
	if string(a) != string(a2) || string(a) == string(b) || len(a) != 32 {
		t.Fatal("derived keys not deterministic and per-team")
	}
	if got := string(mustNew(t, keyA, "value-123456").RedactBytes([]byte("v value-123456"))); strings.Contains(got, "value-123456") {
		t.Fatalf("RedactBytes: %q", got)
	}
}
