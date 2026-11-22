package github_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/github"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

// fixture reads a recorded GitHub response.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeGitHub replays recorded GitHub answers: the app endpoints, and pull requests with ETags.
type fakeGitHub struct {
	t   testing.TB
	srv *httptest.Server
	clk *clock

	mu          sync.Mutex
	requests    int
	tokens      int
	lookups     int
	conditional int
	notModified int
	pull        []byte                                            // the body served for every pull request
	version     int                                               // changes the ETag
	override    func(w http.ResponseWriter, r *http.Request) bool // returns true when it answered
	tokenTTL    time.Duration
	tokenSeq    int
	badTokens   map[string]bool
	lastAuth    string
	gotJWT      string
}

func newFakeGitHub(t testing.TB, clk *clock) *fakeGitHub {
	f := &fakeGitHub{t: t, clk: clk, pull: fixture(t, "pull_open.json"), version: 1, tokenTTL: time.Hour}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if f.override != nil && f.override(w, r) {
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/orgs/acme/installation":
		f.lookups++
		f.gotJWT = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		_, _ = w.Write([]byte(`{"id":7890123,"app_id":123456}`))
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7890123/access_tokens":
		f.tokens++
		f.gotJWT = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.tokenSeq++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_example%d","expires_at":%q}`, f.tokenSeq, f.clk.Now().Add(f.tokenTTL).Format(time.RFC3339))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/"):
		f.lastAuth = r.Header.Get("Authorization")
		if f.badTokens[strings.TrimPrefix(f.lastAuth, "Bearer ")] {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		etag := fmt.Sprintf(`W/"v%d-%s"`, f.version, r.URL.Path)
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			f.conditional++
			if inm == etag {
				f.notModified++
				w.Header().Set("ETag", etag)
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(f.pull)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) client(clk *clock, max int) *github.Client {
	return github.New(github.Options{
		Apps: []github.App{{Host: "github.com", Org: "acme", AppID: "123456", Key: testKey, APIURL: f.srv.URL}},
		Now:  clk.Now, MaxPerHour: max,
	})
}

func ref(repo string, n int) workorder.PRRef {
	return workorder.PRRef{Host: "github.com", Owner: "acme", Repo: repo, Number: n}
}

func TestPullRequestStates(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    workorder.PRState
	}{
		{"pull_open.json", workorder.PRState{State: "open", URL: "https://github.com/acme/api/pull/7"}},
		{"pull_merged.json", workorder.PRState{State: "closed", Merged: true, MergedAt: time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC), URL: "https://github.com/acme/api/pull/7"}},
		{"pull_closed.json", workorder.PRState{State: "closed", URL: "https://github.com/acme/api/pull/7"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
			f := newFakeGitHub(t, clk)
			f.pull = fixture(t, tc.fixture)
			st, err := f.client(clk, 0).PullRequest(t.Context(), ref("api", 7))
			if err != nil {
				t.Fatal(err)
			}
			if st != tc.want {
				t.Errorf("state = %+v, want %+v", st, tc.want)
			}
		})
	}
}

// RC-US-10.4: the app JWT is RS256, names the app, lives at most ten minutes, and the installation token is
// obtained once and reused until just before it expires.
func TestAuthentication(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	f := newFakeGitHub(t, clk)
	c := f.client(clk, 0)
	for i := 0; i < 3; i++ {
		if _, err := c.PullRequest(t.Context(), ref("api", 7)); err != nil {
			t.Fatal(err)
		}
	}
	if f.lookups != 1 || f.tokens != 1 {
		t.Errorf("installation lookups = %d, tokens = %d: one each, then reused", f.lookups, f.tokens)
	}
	if f.lastAuth != "Bearer ghs_example1" {
		t.Errorf("pull request auth = %q", f.lastAuth)
	}
	verifyJWT(t, f.gotJWT, clk.Now())

	// Within a minute of expiry the token is renewed.
	clk.Advance(f.tokenTTL - 30*time.Second)
	if _, err := c.PullRequest(t.Context(), ref("api", 7)); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 2 || f.lastAuth != "Bearer ghs_example2" {
		t.Errorf("tokens = %d, auth = %q: the token is renewed before it expires", f.tokens, f.lastAuth)
	}
	// The installation id from the apps file is used without a lookup.
	c2 := github.New(github.Options{Apps: []github.App{{Host: "github.com", Org: "acme", AppID: "123456", InstallationID: 7890123, Key: testKey, APIURL: f.srv.URL}}, Now: clk.Now})
	l := f.lookups
	if _, err := c2.PullRequest(t.Context(), ref("api", 7)); err != nil || f.lookups != l {
		t.Errorf("lookups = %d (was %d), err = %v", f.lookups, l, err)
	}
}

func verifyJWT(t *testing.T, tok string, now time.Time) {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", tok)
	}
	var head struct{ Alg, Typ string }
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	dec := func(s string, v any) {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	dec(parts[0], &head)
	dec(parts[1], &claims)
	if head.Alg != "RS256" || head.Typ != "JWT" || claims.Iss != "123456" {
		t.Errorf("head = %+v claims = %+v", head, claims)
	}
	if claims.Exp-now.Unix() > 600 || claims.Exp <= now.Unix() || claims.Iat > now.Unix() {
		t.Errorf("iat %d exp %d now %d: GitHub allows ten minutes at most", claims.Iat, claims.Exp, now.Unix())
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, h[:], sig); err != nil {
		t.Errorf("signature: %v", err)
	}
}

func TestConditionalRequestsAnswerWithTheLastState(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	f := newFakeGitHub(t, clk)
	c := f.client(clk, 0)
	first, err := c.PullRequest(t.Context(), ref("api", 7))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.PullRequest(t.Context(), ref("api", 7))
	if err != nil || second != first || f.conditional != 1 || f.notModified != 1 {
		t.Fatalf("second = %+v, %v (conditional %d, 304s %d)", second, err, f.conditional, f.notModified)
	}
	// The pull request changes: a new ETag, the full answer.
	f.version++
	f.pull = fixture(t, "pull_merged.json")
	third, err := c.PullRequest(t.Context(), ref("api", 7))
	if err != nil || !third.Merged {
		t.Fatalf("third = %+v, %v", third, err)
	}
	// Another pull request does not share the cache.
	if _, err := c.PullRequest(t.Context(), ref("api", 8)); err != nil || f.conditional != 2 {
		t.Errorf("conditional = %d: a different pull request is a plain request (%v)", f.conditional, err)
	}
}

// RC-US-10.4: 500 repositories every 10 minutes stay under 5,000 requests per hour per installation, with and
// without ETags helping.
func TestFiveHundredRepositoriesEveryTenMinutesStayUnderTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes bool // every pull request changes each cycle, so no 304 helps
	}{{"conditional requests", false}, {"every answer changes", true}} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
			f := newFakeGitHub(t, clk)
			c := f.client(clk, 0)
			for cycle := 0; cycle < 7; cycle++ { // seven polls span more than an hour
				if tc.changes {
					f.mu.Lock()
					f.version++
					f.mu.Unlock()
				}
				for n := 1; n <= 500; n++ {
					if _, err := c.PullRequest(t.Context(), ref("r"+strconv.Itoa(n), n)); err != nil {
						t.Fatalf("cycle %d repo %d: %v", cycle, n, err)
					}
				}
				clk.Advance(10 * time.Minute)
				if cycle == 5 { // the first hour is over
					f.mu.Lock()
					inHour := f.requests
					f.mu.Unlock()
					if inHour >= 5000 {
						t.Errorf("%d requests in the first hour (limit 5,000)", inHour)
					}
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !tc.changes && f.notModified == 0 {
				t.Error("no conditional request was answered 304")
			}
			t.Logf("%d requests in 70 minutes (%d tokens, %d 304s)", f.requests, f.tokens, f.notModified)
		})
	}
}

// The adapter's own hourly budget holds even when more repositories are asked for than the hour allows.
func TestTheHourlyBudgetStopsRequests(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	f := newFakeGitHub(t, clk)
	c := f.client(clk, 100)
	var blocked *github.BackoffError
	ok := 0
	for n := 1; n <= 150; n++ {
		_, err := c.PullRequest(t.Context(), ref("r"+strconv.Itoa(n), n))
		switch {
		case err == nil:
			ok++
		case errors.As(err, &blocked):
		default:
			t.Fatal(err)
		}
	}
	if blocked == nil || ok >= 100 || f.requests > 100 {
		t.Fatalf("ok = %d, requests = %d, blocked = %v: the budget counts every call, tokens included", ok, f.requests, blocked)
	}
	if want := clk.Now().Add(time.Hour); !blocked.RetryAt().Equal(want) || blocked.Scope() != "github.com/acme" {
		t.Errorf("retry at %v scope %q, want %v: the oldest call leaves the window then", blocked.RetryAt(), blocked.Scope(), want)
	}
	// An hour on, the budget is back.
	clk.Advance(time.Hour + time.Second)
	if _, err := c.PullRequest(t.Context(), ref("again", 1)); err != nil {
		t.Errorf("after the hour: %v", err)
	}
}

// RC-US-10.4: a rate limit or a 5xx backs off until the reset time and keeps the last known state.
func TestRateLimitsAndServerErrorsBackOff(t *testing.T) {
	reset := time.Date(2026, 11, 22, 10, 40, 0, 0, time.UTC)
	cases := []struct {
		name    string
		answer  func(w http.ResponseWriter)
		retryAt func(now time.Time) time.Time
	}{
		{"primary 403 with the reset header", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(fixture(t, "rate_limited.json"))
		}, func(time.Time) time.Time { return reset }},
		{"429 with the reset header", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write(fixture(t, "rate_limited.json"))
		}, func(time.Time) time.Time { return reset }},
		{"secondary limit with Retry-After", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(fixture(t, "secondary_limit.json"))
		}, func(now time.Time) time.Time { return now.Add(2 * time.Minute) }},
		{"secondary limit without a header", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(fixture(t, "secondary_limit.json"))
		}, func(now time.Time) time.Time { return now.Add(time.Minute) }},
		{"500", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, func(now time.Time) time.Time { return now.Add(time.Minute) }},
		{"502", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, func(now time.Time) time.Time { return now.Add(time.Minute) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
			f := newFakeGitHub(t, clk)
			c := f.client(clk, 0)
			good, err := c.PullRequest(t.Context(), ref("api", 7))
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasPrefix(r.URL.Path, "/repos/") {
					return false
				}
				tc.answer(w)
				return true
			}
			f.mu.Unlock()
			clk.Advance(10 * time.Minute)
			now := clk.Now()
			_, err = c.PullRequest(t.Context(), ref("api", 7))
			var b workorder.Backoff
			if !errors.As(err, &b) {
				t.Fatalf("err = %v, want a backoff", err)
			}
			if !b.RetryAt().Equal(tc.retryAt(now)) || b.Scope() != "github.com/acme" {
				t.Errorf("retry at %v scope %q, want %v", b.RetryAt(), b.Scope(), tc.retryAt(now))
			}
			if strings.Contains(err.Error(), "ghs_") {
				t.Errorf("the error leaks a token: %v", err)
			}

			// While blocked: no request leaves, for any pull request of the installation.
			f.mu.Lock()
			before := f.requests
			f.mu.Unlock()
			for _, n := range []int{7, 8} {
				if _, err := c.PullRequest(t.Context(), ref("api", n)); !errors.As(err, &b) {
					t.Errorf("blocked call %d: %v", n, err)
				}
			}
			f.mu.Lock()
			if f.requests != before {
				t.Errorf("%d requests left while blocked", f.requests-before)
			}
			f.override = nil
			f.mu.Unlock()

			// Once the time has come the installation reads again, and the last known state is still valid.
			clk.Advance(b.RetryAt().Sub(clk.Now()) + time.Second)
			st, err := c.PullRequest(t.Context(), ref("api", 7))
			if err != nil || st != good {
				t.Errorf("after the block: %+v, %v", st, err)
			}
		})
	}
}

func TestServerErrorBackoffGrows(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	f := newFakeGitHub(t, clk)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	c := f.client(clk, 0)
	var last time.Duration
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		_, err := c.PullRequest(t.Context(), ref("api", 7))
		var b workorder.Backoff
		if !errors.As(err, &b) {
			t.Fatalf("round %d: %v", i, err)
		}
		last = b.RetryAt().Sub(clk.Now())
		if last != want {
			t.Errorf("round %d: delay %v, want %v", i, last, want)
		}
		clk.Advance(last + time.Second)
	}
}

func TestOtherAnswers(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	f := newFakeGitHub(t, clk)
	c := f.client(clk, 0)

	// No app for the organization or host.
	for _, r := range []workorder.PRRef{{Host: "github.com", Owner: "other", Repo: "x", Number: 1}, {Host: "ghe.example.com", Owner: "acme", Repo: "x", Number: 1}} {
		if _, err := c.PullRequest(t.Context(), r); !errors.Is(err, workorder.ErrNoInstallation) {
			t.Errorf("%v: %v", r, err)
		}
	}
	// A plain 403 is an error, not a backoff.
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/repos/acme/denied") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(fixture(t, "forbidden.json"))
			return true
		}
		if strings.HasPrefix(r.URL.Path, "/repos/acme/gone") {
			http.NotFound(w, r)
			return true
		}
		return false
	}
	var b workorder.Backoff
	if _, err := c.PullRequest(t.Context(), ref("denied", 1)); err == nil || errors.As(err, &b) || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("403: %v", err)
	}
	if _, err := c.PullRequest(t.Context(), ref("gone", 1)); err == nil || errors.As(err, &b) || !strings.Contains(err.Error(), "not found") {
		t.Errorf("404: %v", err)
	}
	// A refused token is replaced once.
	f.override = nil
	f.badTokens = map[string]bool{"ghs_example1": true}
	st, err := c.PullRequest(t.Context(), ref("api", 7))
	if err != nil || st.State != "open" || f.tokens != 2 {
		t.Errorf("after a 401: %+v, %v, tokens = %d", st, err, f.tokens)
	}
}

func TestAppCallFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		block  bool
		text   string
	}{
		{"not installed", http.StatusNotFound, false, "not installed"},
		{"bad credentials", http.StatusUnauthorized, false, "credentials were refused"},
		{"server error", http.StatusBadGateway, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &clock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
			f := newFakeGitHub(t, clk)
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasPrefix(r.URL.Path, "/repos/") {
					w.WriteHeader(tc.status)
					return true
				}
				return false
			}
			_, err := f.client(clk, 0).PullRequest(t.Context(), ref("api", 7))
			var b workorder.Backoff
			if err == nil || errors.As(err, &b) != tc.block || (tc.text != "" && !strings.Contains(err.Error(), tc.text)) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestLoadApps(t *testing.T) {
	dir := t.TempDir()
	der := x509.MarshalPKCS1PrivateKey(testKey)
	if err := os.WriteFile(filepath.Join(dir, "acme.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	pk8, _ := x509.MarshalPKCS8PrivateKey(testKey)
	if err := os.WriteFile(filepath.Join(dir, "ghe.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk8}), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(s string) string {
		p := filepath.Join(dir, "apps.json")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	apps, err := github.LoadApps(write(`{"apps":[
	  {"host":"GitHub.com","org":"acme","appId":"123456","installationId":7890123,"privateKeyFile":"acme.pem"},
	  {"host":"ghe.example.com","org":"platform","appId":"9","privateKeyFile":"` + filepath.Join(dir, "ghe.pem") + `","apiUrl":"https://ghe.example.com/api/v3/"}]}`))
	if err != nil || len(apps) != 2 {
		t.Fatalf("apps = %+v, %v", apps, err)
	}
	if apps[0].Host != "github.com" || apps[0].InstallationID != 7890123 || apps[0].Key == nil || apps[1].APIURL != "https://ghe.example.com/api/v3" {
		t.Errorf("apps = %+v", apps)
	}

	if _, err := github.LoadApps(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing file is an error")
	}
	for name, body := range map[string]string{
		"not json":      `{`,
		"unknown field": `{"apps":[{"host":"github.com","org":"a","appId":"1","privateKeyFile":"acme.pem","token":"x"}]}`,
		"missing field": `{"apps":[{"host":"github.com","org":"a","privateKeyFile":"acme.pem"}]}`,
		"duplicate":     `{"apps":[{"host":"github.com","org":"a","appId":"1","privateKeyFile":"acme.pem"},{"host":"GITHUB.com","org":"A","appId":"2","privateKeyFile":"acme.pem"}]}`,
		"no key file":   `{"apps":[{"host":"github.com","org":"a","appId":"1","privateKeyFile":"nope.pem"}]}`,
	} {
		if _, err := github.LoadApps(write(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.pem"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := github.LoadApps(write(`{"apps":[{"host":"github.com","org":"a","appId":"1","privateKeyFile":"bad.pem"}]}`)); err == nil {
		t.Error("a bad key is accepted")
	}
	if _, err := github.ParseKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")})); err == nil {
		t.Error("a non-key PEM is accepted")
	}
}
