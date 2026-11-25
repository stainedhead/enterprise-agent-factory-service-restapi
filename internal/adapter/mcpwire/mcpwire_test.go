package mcpwire_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		code int // 0 = ok
		kind string
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 0, "request"},
		{`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"x"}}`, 0, "request"},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 0, "notification"},
		{`{"jsonrpc":"2.0","id":-3,"result":{}}`, 0, "response"},
		{`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"x"}}`, 0, "response"},
		{`[{"jsonrpc":"2.0","id":1,"method":"a"}]`, mcpwire.CodeInvalidRequest, ""},
		{`{`, mcpwire.CodeParseError, ""},
		{`{"jsonrpc":"2.0","id":1,"method":"a"} {}`, mcpwire.CodeParseError, ""},
		{`{"jsonrpc":"1.0","id":1,"method":"a"}`, mcpwire.CodeInvalidRequest, ""},
		{`{"jsonrpc":"2.0","id":{"x":1},"method":"a"}`, mcpwire.CodeInvalidRequest, ""},
		{`{"jsonrpc":"2.0","id":1,"method":"a","result":{}}`, mcpwire.CodeInvalidRequest, ""},
		{`{"jsonrpc":"2.0"}`, mcpwire.CodeInvalidRequest, ""},
		{`{"jsonrpc":"2.0","id":1}`, mcpwire.CodeInvalidRequest, ""},
		{`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"m"}}`, mcpwire.CodeInvalidRequest, ""},
	}
	for _, tt := range tests {
		m, err := mcpwire.Parse([]byte(tt.in))
		if tt.code != 0 {
			if err == nil || err.Code != tt.code {
				t.Errorf("%s: got %v want code %d", tt.in, err, tt.code)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.in, err)
			continue
		}
		got := map[bool]string{m.IsRequest(): "request", m.IsNotification(): "notification", m.IsResponse(): "response"}[true]
		if got != tt.kind {
			t.Errorf("%s: kind %s want %s", tt.in, got, tt.kind)
		}
	}
	if !strings.Contains((&mcpwire.Error{Code: 1, Message: "m"}).Error(), "m") {
		t.Error("Error()")
	}
}

func TestBuilders(t *testing.T) {
	r, err := mcpwire.NewResult(json.RawMessage(`7`), map[string]int{"a": 1})
	if err != nil || string(r.Result) != `{"a":1}` || string(r.ID) != "7" {
		t.Fatalf("NewResult %+v %v", r, err)
	}
	if _, err := mcpwire.NewResult(nil, func() {}); err == nil {
		t.Error("unmarshalable result")
	}
	e := mcpwire.NewError(nil, mcpwire.CodeInvalidParams, "Unknown tool", map[string]string{"reason": "x"})
	if string(e.ID) != "null" || e.Error.Code != -32602 || string(e.Error.Data) != `{"reason":"x"}` {
		t.Errorf("NewError %+v", e)
	}
	if e := mcpwire.NewError(json.RawMessage(`1`), 1, "m", nil); e.Error.Data != nil {
		t.Error("nil data")
	}
	q, err := mcpwire.NewRequest(json.RawMessage(`1`), "tools/list", nil)
	if err != nil || q.Params != nil || !q.IsRequest() {
		t.Errorf("NewRequest %+v", q)
	}
	if _, err := mcpwire.NewRequest(json.RawMessage(`1`), "x", func() {}); err == nil {
		t.Error("unmarshalable params")
	}
	if !mcpwire.IsSupported("2026-07-28") || !mcpwire.IsSupported("2025-11-25") || mcpwire.IsSupported("2025-06-18") {
		t.Error("IsSupported")
	}
}

func req(method, params string) *mcpwire.Message {
	return &mcpwire.Message{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: json.RawMessage(params)}
}

func TestNameOf(t *testing.T) {
	tests := []struct{ method, params, want string }{
		{"tools/call", `{"name":"create_issue","arguments":{}}`, "create_issue"},
		{"prompts/get", `{"name":"review"}`, "review"},
		{"resources/read", `{"uri":"repo://a/b"}`, "repo://a/b"},
		{"resources/subscribe", `{"uri":"x"}`, "x"},
		{"tools/list", `{}`, ""},
		{"tools/call", `not json`, ""},
		{"tools/call", `{"name":5}`, ""},
	}
	for _, tt := range tests {
		if got := mcpwire.NameOf(tt.method, json.RawMessage(tt.params)); got != tt.want {
			t.Errorf("%s %s: %q", tt.method, tt.params, got)
		}
	}
}

func TestCheckHeaders(t *testing.T) {
	call := req("tools/call", `{"name":"x"}`)
	list := req("tools/list", `{}`)
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	tests := []struct {
		name string
		h    http.Header
		m    *mcpwire.Message
		bad  bool
	}{
		{"2026 ok", h("MCP-Protocol-Version", "2026-07-28", "Mcp-Method", "tools/call", "Mcp-Name", "x"), call, false},
		{"2026 list no name", h("MCP-Protocol-Version", "2026-07-28", "Mcp-Method", "tools/list"), list, false},
		{"2026 missing method", h("MCP-Protocol-Version", "2026-07-28"), call, true},
		{"2026 missing name", h("MCP-Protocol-Version", "2026-07-28", "Mcp-Method", "tools/call"), call, true},
		{"method differs", h("Mcp-Method", "tools/list"), call, true},
		{"name differs", h("Mcp-Method", "tools/call", "Mcp-Name", "y"), call, true},
		{"2025 without headers", h(), call, false},
		{"name on list", h("Mcp-Name", "x"), list, true},
	}
	for _, tt := range tests {
		err := mcpwire.CheckHeaders(tt.h, tt.m)
		if (err != nil) != tt.bad {
			t.Errorf("%s: %v", tt.name, err)
		}
		if err != nil && (err.Code != mcpwire.CodeHeaderMismatch || !strings.Contains(string(err.Data), "header_mismatch")) {
			t.Errorf("%s: %+v", tt.name, err)
		}
	}
	if mcpwire.Version(http.Header{}) != mcpwire.Version20251125 {
		t.Error("default version")
	}
}

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestSealer(t *testing.T) {
	s, err := mcpwire.NewSealer(key(1))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Seal(mcpwire.Session{Version: "2025-11-25", Subject: "agt_1", Expires: 1 << 40})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Open(id, "agt_1")
	if err != nil || got.Version != "2025-11-25" {
		t.Fatalf("open %+v %v", got, err)
	}
	if _, err := s.Open(id, "agt_2"); !errors.Is(err, mcpwire.ErrSessionInvalid) {
		t.Errorf("other subject: %v", err)
	}
	for _, bad := range []string{"%%%", "", "AAAA"} {
		if _, err := s.Open(bad, "agt_1"); !errors.Is(err, mcpwire.ErrSessionInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	old, _ := s.Seal(mcpwire.Session{Subject: "agt_1", Expires: 1})
	if _, err := s.Open(old, "agt_1"); !errors.Is(err, mcpwire.ErrSessionExpired) {
		t.Errorf("expired: %v", err)
	}
	rotated, _ := mcpwire.NewSealer(key(2), key(1))
	if _, err := rotated.Open(id, "agt_1"); err != nil {
		t.Errorf("previous key must still open: %v", err)
	}
	other, _ := mcpwire.NewSealer(key(3))
	if _, err := other.Open(id, "agt_1"); !errors.Is(err, mcpwire.ErrSessionInvalid) {
		t.Errorf("wrong key: %v", err)
	}
	for _, ks := range [][][]byte{nil, {key(1), key(2), key(3)}, {[]byte("short")}} {
		if _, err := mcpwire.NewSealer(ks...); err == nil {
			t.Errorf("keys %d: want error", len(ks))
		}
	}
}

func TestClientJSONAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Method") != "tools/call" || r.Header.Get("Mcp-Name") != "x" ||
			r.Header.Get("MCP-Protocol-Version") != "2026-07-28" || r.Header.Get("Authorization") != "Bearer up" ||
			r.Header.Get("Mcp-Session-Id") != "sid" {
			http.Error(w, "bad headers", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`)
	}))
	defer srv.Close()
	c := &mcpwire.Client{URL: srv.URL, Version: "2026-07-28", Header: func(h http.Header) { h.Set("Authorization", "Bearer up") }}
	resp, err := c.Call(context.Background(), req("tools/call", `{"name":"x"}`), "sid", nil)
	if err != nil || !resp.IsResponse() {
		t.Fatalf("%+v %v", resp, err)
	}
}

func TestClientStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": comment\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\n")
		_, _ = fmt.Fprint(w, "data:  \"id\":1,\"result\":{\"ok\":true}}\n\n")
	}))
	defer srv.Close()
	var others int
	c := &mcpwire.Client{URL: srv.URL}
	resp, err := c.Call(context.Background(), req("tools/list", `{}`), "", func(*mcpwire.Message) { others++ })
	if err != nil || string(resp.Result) != `{"ok":true}` || others != 2 {
		t.Fatalf("%+v %v others=%d", resp, err, others)
	}
}

func TestClientStreamEnds(t *testing.T) {
	for name, body := range map[string]string{
		"no trailing blank": "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}",
		"eof":               "data: {\"jsonrpc\":\"2.0\",\"id\":9,\"result\":{}}\n\n",
		"bad event":         "data: {nope\n\n",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = io.WriteString(w, body)
		}))
		resp, err := (&mcpwire.Client{URL: srv.URL}).Call(context.Background(), req("tools/list", `{}`), "", nil)
		switch name {
		case "no trailing blank":
			if err != nil || resp == nil {
				t.Errorf("%s: %v", name, err)
			}
		case "eof":
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("%s: %v", name, err)
			}
		default:
			if err == nil {
				t.Errorf("%s: want error", name)
			}
		}
		srv.Close()
	}
}

func TestClientErrors(t *testing.T) {
	handler := func(status int, ct, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	srv := httptest.NewServer(handler(429, "", "slow down"))
	_, err := (&mcpwire.Client{URL: srv.URL}).Call(context.Background(), req("tools/list", `{}`), "", nil)
	var he *mcpwire.HTTPError
	if !errors.As(err, &he) || he.Status != 429 || he.RetryAfter != "3" || he.Error() == "" {
		t.Errorf("429: %v", err)
	}
	srv.Close()

	srv = httptest.NewServer(handler(202, "", ""))
	note := &mcpwire.Message{JSONRPC: "2.0", Method: "notifications/initialized"}
	if resp, err := (&mcpwire.Client{URL: srv.URL}).Call(context.Background(), note, "", nil); resp != nil || err != nil {
		t.Errorf("notification: %v %v", resp, err)
	}
	srv.Close()

	srv = httptest.NewServer(handler(200, "text/html", "<p>"))
	if _, err := (&mcpwire.Client{URL: srv.URL}).Call(context.Background(), req("tools/list", `{}`), "", nil); err == nil {
		t.Error("html: want error")
	}
	srv.Close()

	srv = httptest.NewServer(handler(200, "application/json", `{bad`))
	if _, err := (&mcpwire.Client{URL: srv.URL}).Call(context.Background(), req("tools/list", `{}`), "", nil); err == nil {
		t.Error("bad json: want error")
	}
	srv.Close()

	big := `{"jsonrpc":"2.0","id":1,"result":{"x":"` + strings.Repeat("a", 100) + `"}}`
	srv = httptest.NewServer(handler(200, "application/json", big))
	if _, err := (&mcpwire.Client{URL: srv.URL, MaxResponse: 50}).Call(context.Background(), req("tools/list", `{}`), "", nil); !errors.Is(err, mcpwire.ErrResponseTooLarge) {
		t.Errorf("too large: %v", err)
	}
	if _, err := (&mcpwire.Client{URL: srv.URL, MaxResponse: int64(len(big))}).Call(context.Background(), req("tools/list", `{}`), "", nil); err != nil {
		t.Errorf("exact fit: %v", err)
	}
	srv.Close()

	if _, err := (&mcpwire.Client{URL: "http://127.0.0.1:1"}).Call(context.Background(), req("tools/list", `{}`), "", nil); err == nil {
		t.Error("unreachable: want error")
	}
	if _, err := (&mcpwire.Client{URL: "::bad"}).Call(context.Background(), req("tools/list", `{}`), "", nil); err == nil {
		t.Error("bad url: want error")
	}
	bad := &mcpwire.Message{JSONRPC: "2.0", ID: json.RawMessage(`{`), Method: "x"}
	if _, err := (&mcpwire.Client{URL: "http://x"}).Call(context.Background(), bad, "", nil); err == nil {
		t.Error("unmarshalable message: want error")
	}
}
