package notifyhttp_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/notifyhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

var msg = notify.Message{ChannelID: "nch_1", TeamID: "team_1", EventID: "ntest_1", EventType: "notification.test_requested", Text: "hello", Detail: json.RawMessage(`{"a":1}`)}

func TestSlackFakeServer(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		if r.URL.Path == "/fail" {
			http.Error(w, "no_service", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	s := notifyhttp.New(notifyhttp.Options{AllowPrivate: true})
	ch := notify.Channel{Kind: domain.ChannelSlack, Config: map[string]any{"channel": "#alerts"}}
	if err := s.Send(t.Context(), ch, notify.Credential(srv.URL+"/hook"), msg); err != nil || got["text"] != "hello" || got["channel"] != "#alerts" {
		t.Fatalf("%v %v", err, got)
	}
	// A failing Slack returns the failure without the webhook URL (RA-US-17.3).
	err := s.Send(t.Context(), ch, notify.Credential(srv.URL+"/fail"), msg)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "no_service") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("%v", err)
	}
}

func TestWebhookAndJira(t *testing.T) {
	var auth, user, method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		auth, method, path = r.Header.Get("Authorization"), r.Method, r.URL.Path
		user, _, _ = r.BasicAuth()
	}))
	defer srv.Close()
	s := notifyhttp.New(notifyhttp.Options{AllowPrivate: true})
	if err := s.Send(t.Context(), notify.Channel{Kind: domain.ChannelWebhook, Config: map[string]any{"url": srv.URL + "/in"}}, "Bearer abc", msg); err != nil ||
		auth != "Bearer abc" || method != http.MethodPost || path != "/in" {
		t.Fatalf("%v %q %q %q", err, auth, method, path)
	}
	if err := s.Send(t.Context(), notify.Channel{Kind: domain.ChannelJira, Config: map[string]any{"url": srv.URL + "/"}}, "me@example.com:tok", msg); err != nil ||
		user != "me@example.com" || method != http.MethodGet || path != "/rest/api/2/myself" {
		t.Fatalf("%v %q %q %q", err, user, method, path)
	}
	if err := s.Send(t.Context(), notify.Channel{Kind: domain.ChannelJira, Config: map[string]any{"url": srv.URL}}, "nocolon", msg); err == nil {
		t.Fatal("want error")
	}
	if err := s.Send(t.Context(), notify.Channel{Kind: domain.ChannelEmail}, "", msg); err == nil {
		t.Fatal("email is not HTTP")
	}
}

func TestRefusesUnsafeTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("must not be called") }))
	defer srv.Close()
	s := notifyhttp.New(notifyhttp.Options{}) // production settings
	wh := func(u string) notify.Channel { return notify.Channel{Kind: domain.ChannelWebhook, Config: map[string]any{"url": u}} }
	for name, u := range map[string]string{
		"plain http":       srv.URL,
		"loopback https":   strings.Replace(srv.URL, "http://", "https://", 1),
		"metadata address": "https://169.254.169.254/latest/meta-data",
		"private address":  "https://10.0.0.5/x",
		"ipv6 loopback":    "https://[::1]/x",
		"credentials":      "https://user:pw@example.com/x",
		"not a url":        "::",
	} {
		if err := s.Send(t.Context(), wh(u), "", msg); err == nil {
			t.Errorf("%s: want refusal", name)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer srv.Close()
	err := notifyhttp.New(notifyhttp.Options{AllowPrivate: true}).Send(t.Context(), notify.Channel{Kind: domain.ChannelWebhook, Config: map[string]any{"url": srv.URL}}, "", msg)
	if err == nil || hit {
		t.Fatalf("redirect followed: err=%v hit=%v", err, hit)
	}
}
