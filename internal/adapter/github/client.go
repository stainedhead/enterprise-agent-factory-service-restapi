package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

// Defaults.
const (
	// DefaultMaxPerHour is the request budget per installation and hour: GitHub allows 5,000, and the margin
	// covers token requests and anything else the installation does.
	DefaultMaxPerHour = 4500
	// jwtTTL is the App JWT's lifetime; GitHub allows ten minutes.
	jwtTTL = 9 * time.Minute
	// tokenSlack renews an installation token this long before it expires.
	tokenSlack = time.Minute
	// maxBody bounds what is read of a response.
	maxBody = 1 << 20

	serverBackoffBase = time.Minute
	serverBackoffMax  = 30 * time.Minute
	// minRateBlock keeps a reset time in the past or a zero Retry-After from becoming a hot loop.
	minRateBlock = 10 * time.Second
)

// Options configure a Client.
type Options struct {
	// Apps are the installations.
	Apps []App
	// HTTP is the HTTP client (default: 15 s timeout).
	HTTP *http.Client
	// Now is the clock (default time.Now).
	Now func() time.Time
	// MaxPerHour is the request budget per installation and hour (default DefaultMaxPerHour).
	MaxPerHour int
}

// Client implements workorder.GitHub.
type Client struct {
	http *http.Client
	now  func() time.Time
	max  int
	inst map[string]*installation
}

var _ workorder.GitHub = (*Client)(nil)

// installation is one app's state: its token, the ETag cache, the budget window and any block.
type installation struct {
	app App

	mu           sync.Mutex
	token        string
	tokenExpires time.Time
	instID       int64
	calls        []time.Time // request times in the last hour
	blockedUntil time.Time
	blockReason  string
	serverFails  int
	cache        map[string]cached
}

type cached struct {
	etag  string
	state workorder.PRState
}

// New returns a Client for the apps.
func New(o Options) *Client {
	c := &Client{http: o.HTTP, now: o.Now, max: o.MaxPerHour, inst: map[string]*installation{}}
	if c.http == nil {
		c.http = &http.Client{Timeout: 15 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.max <= 0 {
		c.max = DefaultMaxPerHour
	}
	for _, a := range o.Apps {
		a.Host = strings.ToLower(a.Host)
		c.inst[scopeKey(a.Host, a.Org)] = &installation{app: a, instID: a.InstallationID, cache: map[string]cached{}}
	}
	return c
}

// BackoffError is returned when an installation is blocked: GitHub limited it, failed, or its own hourly budget
// is used. It implements workorder.Backoff. The message never contains a token.
type BackoffError struct {
	scope string
	at    time.Time
	why   string
}

// Error implements error.
func (e *BackoffError) Error() string {
	return fmt.Sprintf("github: %s is blocked until %s: %s", e.scope, e.at.UTC().Format(time.RFC3339), e.why)
}

// RetryAt implements workorder.Backoff.
func (e *BackoffError) RetryAt() time.Time { return e.at }

// Scope implements workorder.Backoff.
func (e *BackoffError) Scope() string { return e.scope }

// PullRequest implements workorder.GitHub.
func (c *Client) PullRequest(ctx context.Context, ref workorder.PRRef) (workorder.PRState, error) {
	in, ok := c.inst[scopeKey(ref.Host, ref.Owner)]
	if !ok {
		return workorder.PRState{}, workorder.ErrNoInstallation
	}
	return in.pullRequest(ctx, c, ref)
}

func (in *installation) scope() string { return scopeKey(in.app.Host, in.app.Org) }

func (in *installation) apiBase() string {
	if in.app.APIURL != "" {
		return in.app.APIURL
	}
	if in.app.Host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + in.app.Host + "/api/v3"
}

// admit enforces the block and the hourly budget for one more request. It must be called with in.mu held.
func (in *installation) admit(now time.Time, limit int) error {
	if now.Before(in.blockedUntil) {
		return &BackoffError{scope: in.scope(), at: in.blockedUntil, why: in.blockReason}
	}
	cut := now.Add(-time.Hour)
	i := 0
	for i < len(in.calls) && !in.calls[i].After(cut) {
		i++
	}
	in.calls = in.calls[i:]
	if len(in.calls) >= limit {
		return &BackoffError{scope: in.scope(), at: in.calls[0].Add(time.Hour), why: "the installation's request budget for the hour is used"}
	}
	in.calls = append(in.calls, now)
	return nil
}

func (in *installation) block(until time.Time, why string) *BackoffError {
	in.blockedUntil, in.blockReason = until, why
	return &BackoffError{scope: in.scope(), at: until, why: why}
}

func (in *installation) pullRequest(ctx context.Context, c *Client, ref workorder.PRRef) (workorder.PRState, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if err := in.ensureToken(ctx, c); err != nil {
			return workorder.PRState{}, err
		}
		if err := in.admit(c.now(), c.max); err != nil {
			return workorder.PRState{}, err
		}
		key := fmt.Sprintf("%s/%s#%d", ref.Owner, ref.Repo, ref.Number)
		u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", in.apiBase(), url.PathEscape(ref.Owner), url.PathEscape(ref.Repo), ref.Number)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return workorder.PRState{}, fmt.Errorf("github: %s: %w", ref, err)
		}
		in.headers(req, "Bearer "+in.token)
		prev, hasPrev := in.cache[key]
		if hasPrev && prev.etag != "" {
			req.Header.Set("If-None-Match", prev.etag)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return workorder.PRState{}, fmt.Errorf("github: %s: %w", ref, errNoSecrets(err))
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			var pr struct {
				State    string     `json:"state"`
				Merged   bool       `json:"merged"`
				MergedAt *time.Time `json:"merged_at"`
				HTMLURL  string     `json:"html_url"`
			}
			if err := json.Unmarshal(body, &pr); err != nil {
				return workorder.PRState{}, fmt.Errorf("github: %s: unreadable answer", ref)
			}
			st := workorder.PRState{State: pr.State, Merged: pr.Merged, URL: pr.HTMLURL}
			if pr.MergedAt != nil {
				st.MergedAt = pr.MergedAt.UTC()
			}
			in.serverFails = 0
			if et := resp.Header.Get("ETag"); et != "" {
				in.cache[key] = cached{etag: et, state: st}
			}
			return st, nil
		case resp.StatusCode == http.StatusNotModified && hasPrev:
			in.serverFails = 0
			return prev.state, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			in.token = "" // revoked or expired early: one fresh token, then give up
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden:
			if until, why, limited := rateLimit(resp, body, c.now()); limited {
				return workorder.PRState{}, in.block(until, why)
			}
			return workorder.PRState{}, fmt.Errorf("github: %s: forbidden (%s)", ref, apiMessage(body))
		case resp.StatusCode >= 500:
			in.serverFails++
			d := min(serverBackoffBase<<min(in.serverFails-1, 10), serverBackoffMax)
			return workorder.PRState{}, in.block(c.now().Add(d), fmt.Sprintf("GitHub answered %d", resp.StatusCode))
		case resp.StatusCode == http.StatusNotFound:
			return workorder.PRState{}, fmt.Errorf("github: %s: not found, or the installation cannot see the repository", ref)
		}
		return workorder.PRState{}, fmt.Errorf("github: %s: unexpected status %d", ref, resp.StatusCode)
	}
}

// rateLimit decides whether a 403 or 429 is a rate limit, and until when to stay away.
func rateLimit(resp *http.Response, body []byte, now time.Time) (until time.Time, why string, limited bool) {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil && s >= 0 {
			return now.Add(max(time.Duration(s)*time.Second, minRateBlock)), "GitHub asked to retry later", true
		}
	}
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	if remaining == "0" || resp.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(apiMessage(body)), "rate limit") {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			t := time.Unix(reset, 0).UTC()
			if t.Before(now.Add(minRateBlock)) {
				t = now.Add(minRateBlock)
			}
			return t, "the rate limit is used", true
		}
		return now.Add(time.Minute), "the rate limit is used", true
	}
	return time.Time{}, "", false
}

func apiMessage(body []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) != nil || m.Message == "" {
		return "no message"
	}
	if len(m.Message) > 200 {
		return m.Message[:200]
	}
	return m.Message
}

func (in *installation) headers(req *http.Request, auth string) {
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "afs-reconciler")
}

// ensureToken makes sure in.token is valid for a while. It must be called with in.mu held.
func (in *installation) ensureToken(ctx context.Context, c *Client) error {
	now := c.now()
	if in.token != "" && now.Add(tokenSlack).Before(in.tokenExpires) {
		return nil
	}
	if now.Before(in.blockedUntil) {
		return &BackoffError{scope: in.scope(), at: in.blockedUntil, why: in.blockReason}
	}
	jwt, err := in.appJWT(now)
	if err != nil {
		return err
	}
	if in.instID == 0 {
		var out struct {
			ID int64 `json:"id"`
		}
		if err := in.appCall(ctx, c, http.MethodGet, "/orgs/"+url.PathEscape(in.app.Org)+"/installation", jwt, &out); err != nil {
			return err
		}
		in.instID = out.ID
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := in.appCall(ctx, c, http.MethodPost, "/app/installations/"+strconv.FormatInt(in.instID, 10)+"/access_tokens", jwt, &tok); err != nil {
		return err
	}
	if tok.Token == "" {
		return errors.New("github: the installation token request returned no token")
	}
	in.token, in.tokenExpires = tok.Token, tok.ExpiresAt
	if in.tokenExpires.IsZero() {
		in.tokenExpires = now.Add(time.Hour)
	}
	return nil
}

// appCall makes a call authenticated as the app (JWT). It counts toward the budget and handles limits and
// server errors as PullRequest does.
func (in *installation) appCall(ctx context.Context, c *Client, method, path, jwt string, out any) error {
	if err := in.admit(c.now(), c.max); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, in.apiBase()+path, nil)
	if err != nil {
		return fmt.Errorf("github: %s: %w", in.scope(), err)
	}
	in.headers(req, "Bearer "+jwt)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", in.scope(), errNoSecrets(err))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("github: %s: unreadable answer", in.scope())
		}
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden:
		if until, why, limited := rateLimit(resp, body, c.now()); limited {
			return in.block(until, why)
		}
	case resp.StatusCode >= 500:
		in.serverFails++
		return in.block(c.now().Add(min(serverBackoffBase<<min(in.serverFails-1, 10), serverBackoffMax)), fmt.Sprintf("GitHub answered %d", resp.StatusCode))
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("github: %s: the app is not installed on the organization (404)", in.scope())
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("github: %s: the app's credentials were refused (401): check the app id and private key", in.scope())
	}
	return fmt.Errorf("github: %s: %s answered %d", in.scope(), path, resp.StatusCode)
}

// appJWT signs the app JWT (RS256).
func (in *installation) appJWT(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(jwtTTL).Unix(), "iss": in.app.AppID})
	signing := head + "." + enc.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, in.app.Key, crypto.SHA256, h[:])
	if err != nil {
		return "", errors.New("github: signing the app JWT failed")
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// errNoSecrets drops the URL from a transport error (it can carry nothing secret here, but the error text goes to
// logs and audit, so only the cause is kept).
func errNoSecrets(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
