// Package notifyhttp sends the test messages of notification channels over HTTPS to Slack, a webhook
// or Jira (M3-G4, OQ-S1). It is the one place the reconciler calls a URL a team supplied, so it
// refuses anything but https to a public address: the dialer checks the resolved address (a name that
// resolves to a loopback, private, link-local or metadata address is refused), redirects are not
// followed, and the response is read only to a short, redacted snippet. Credentials are held in
// notify.Credential and sent only in the request; they are never put in an error.
package notifyhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// Options configure a Sender.
type Options struct {
	// Timeout bounds one request (default 10 s).
	Timeout time.Duration
	// AllowPrivate lifts the public-address and https checks. For tests with a local server only.
	AllowPrivate bool
}

// Sender implements notify.HTTPSender.
type Sender struct {
	client *http.Client
	opts   Options
}

var _ notify.HTTPSender = (*Sender)(nil)

// New returns a Sender.
func New(o Options) *Sender {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	if !o.AllowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !public(ap.Addr()) {
				return errors.New("the address is not a public address")
			}
			return nil
		}
	}
	tr := &http.Transport{DialContext: d.DialContext, Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, DisableKeepAlives: true}
	return &Sender{opts: o, client: &http.Client{Transport: tr, Timeout: o.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// public reports whether a is a routable public address.
func public(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && !a.IsLoopback() && !a.IsPrivate() && !a.IsLinkLocalUnicast() && !a.IsLinkLocalMulticast() &&
		!a.IsMulticast() && !a.IsUnspecified() && !a.IsInterfaceLocalMulticast() && a != netip.MustParseAddr("100.100.100.200")
}

type request struct {
	method, url, user, pass, auth string
	body                          any
}

// Send implements notify.HTTPSender.
func (s *Sender) Send(ctx context.Context, ch notify.Channel, cred notify.Credential, m notify.Message) error {
	var r request
	switch ch.Kind {
	case domain.ChannelSlack:
		r = request{method: http.MethodPost, url: cred.Reveal(), body: map[string]string{"channel": domain.ConfigString(ch.Config, "channel"), "text": m.Text}}
	case domain.ChannelWebhook:
		r = request{method: http.MethodPost, url: domain.ConfigString(ch.Config, "url"), auth: cred.Reveal(), body: map[string]any{
			"id": m.EventID, "type": m.EventType, "channelId": m.ChannelID, "teamId": m.TeamID, "text": m.Text, "detail": m.Detail}}
	case domain.ChannelJira:
		// A test must not open an issue: it checks the credential against the user endpoint.
		user, pass, ok := strings.Cut(cred.Reveal(), ":")
		if !ok {
			return errors.New("the Jira secret must hold user:api-token")
		}
		r = request{method: http.MethodGet, url: strings.TrimSuffix(domain.ConfigString(ch.Config, "url"), "/") + "/rest/api/2/myself", user: user, pass: pass}
	default:
		return fmt.Errorf("notifyhttp: %s channels are not delivered over HTTP", ch.Kind)
	}
	return s.do(ctx, ch.Kind, r)
}

func (s *Sender) do(ctx context.Context, kind domain.ChannelKind, r request) error {
	u, err := url.Parse(r.url)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: the target is not a URL", kind)
	}
	if !s.opts.AllowPrivate && (u.Scheme != "https" || u.User != nil) {
		return fmt.Errorf("%s: the target must be an https URL without credentials", kind)
	}
	var body io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return fmt.Errorf("%s: encode message: %w", kind, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		return fmt.Errorf("%s: the target is not a URL", kind)
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.auth != "" {
		req.Header.Set("Authorization", r.auth)
	}
	if r.user != "" {
		req.SetBasicAuth(r.user, r.pass)
	}
	req.Header.Set("User-Agent", "afs-reconciler")
	resp, err := s.client.Do(req)
	if err != nil {
		// The URL error wraps the URL, which may be a webhook credential: report the cause only.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%s: request failed: %s", kind, notify.RedactURLs(err.Error()))
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	msg := notify.RedactURLs(strings.TrimSpace(string(snippet)))
	if len(msg) > 100 {
		msg = msg[:100]
	}
	return fmt.Errorf("%s: HTTP %d %s", kind, resp.StatusCode, msg)
}
