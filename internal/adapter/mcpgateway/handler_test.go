package mcpgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpgateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

type fakeAdmission struct {
	snap *mcpgateway.Snapshot
	err  error
}

func (f *fakeAdmission) Snapshot(context.Context, mcpgateway.Caller) (*mcpgateway.Snapshot, error) {
	return f.snap, f.err
}

type fakeUpstream struct {
	mu    sync.Mutex
	calls []*mcpwire.Message
	fn    func(mcpgateway.Server, *mcpwire.Message) (*mcpwire.Message, error)
}

func (f *fakeUpstream) Call(ctx context.Context, s mcpgateway.Server, m *mcpwire.Message) (*mcpwire.Message, error) {
	f.mu.Lock()
	f.calls = append(f.calls, m)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(s, m)
	}
	return mcpwire.NewResult(m.ID, map[string]any{"content": []any{}, "server": s.Slug})
}

type fakeLedger struct {
	mu sync.Mutex
	ev []mcpgateway.Event
}

func (f *fakeLedger) Record(e mcpgateway.Event) { f.mu.Lock(); f.ev = append(f.ev, e); f.mu.Unlock() }
func (f *fakeLedger) last() mcpgateway.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ev[len(f.ev)-1]
}

func allowed(server, serverID string, kind mcp.Kind, name, desc string) mcpgateway.Entry {
	def := map[string]any{"name": name, "description": desc, "inputSchema": map[string]any{"type": "object"}}
	switch kind {
	case mcp.KindResource:
		def = map[string]any{"uri": name, "name": "r"}
	case mcp.KindResourceTemplate:
		def = map[string]any{"uriTemplate": name, "name": "t"}
	}
	b, _ := json.Marshal(def)
	return mcpgateway.Entry{ServerID: serverID, Definition: b, Digest: "sha256:" + strings.Repeat("a", 64),
		Primitive: mcp.Primitive{Server: server, Kind: kind, Name: name, Description: desc, Status: mcp.StatusApproved,
			Annotations: mcp.Annotations{ReadOnlyHint: mcp.Hint(true)}},
		Decision: mcp.Decision{Allowed: true}}
}

func snapshot() *mcpgateway.Snapshot {
	blocked := allowed("github", "mcp_gh", mcp.KindTool, "delete_repo", "Delete a repository.")
	blocked.Decision = mcp.Decision{Reason: mcp.ReasonBlockedTeam}
	blocked.Reason = mcp.ReasonBlockedTeam
	limited := allowed("github", "mcp_gh", mcp.KindTool, "search_code", "Search code.")
	limited.Decision.RateLimit = 1
	return &mcpgateway.Snapshot{
		Version: "v1", Mode: "search",
		Servers: []mcpgateway.Server{{ID: "mcp_gh", Slug: "github", Kind: "enterprise"}, {ID: "mcp_wiki", Slug: "wiki", Kind: "team"}},
		Entries: []mcpgateway.Entry{
			allowed("github", "mcp_gh", mcp.KindTool, "create_pull_request", "Open a pull request. Needs a branch."),
			allowed("github", "mcp_gh", mcp.KindTool, "list_issues", "List issues in a repository."),
			blocked, limited,
			allowed("wiki", "mcp_wiki", mcp.KindResource, "wiki://home", ""),
			allowed("wiki", "mcp_wiki", mcp.KindResourceTemplate, "wiki://pages/{id}", ""),
			allowed("wiki", "mcp_wiki", mcp.KindPrompt, "summarize", "Summarize a page."),
		},
	}
}

type env struct {
	h   *mcpgateway.Handler
	adm *fakeAdmission
	up  *fakeUpstream
	led *fakeLedger
	sl  *mcpwire.Sealer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	sl, _ := mcpwire.NewSealer(bytes.Repeat([]byte{7}, 32))
	e := &env{adm: &fakeAdmission{snap: snapshot()}, up: &fakeUpstream{}, led: &fakeLedger{}, sl: sl}
	h, err := mcpgateway.New(mcpgateway.Config{
		Identify: func(r *http.Request) (mcpgateway.Caller, error) {
			if r.Header.Get("X-Test-Unknown") != "" {
				return mcpgateway.Caller{}, errors.New("unknown")
			}
			return mcpgateway.Caller{TeamID: "team_1", SubjectKind: "agent", SubjectID: "agt_1"}, nil
		},
		Admission: e.adm, Upstream: e.up, Ledger: e.led, Sealer: sl, Version: "1.3.0",
		Defaults: mcpgateway.Limits{Timeout: 200 * time.Millisecond},
		Boost:    func(context.Context, mcpgateway.Caller) map[string]float64 { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	return e
}

type resp struct {
	status int
	header http.Header
	msg    mcpwire.Message
	raw    string
}

func (e *env) do(t *testing.T, path, body string, hdr ...string) resp {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	if m, err := mcpwire.Parse([]byte(body)); err == nil {
		r.Header.Set("Mcp-Method", m.Method)
		if n := mcpwire.NameOf(m.Method, m.Params); n != "" {
			r.Header.Set("Mcp-Name", n)
		}
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			r.Header.Del(hdr[i])
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	out := resp{status: w.Code, header: w.Header(), raw: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.msg)
	return out
}

func call(id int, method string, params any) string {
	p, _ := json.Marshal(params)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": json.RawMessage(p)})
	return string(b)
}

func names(t *testing.T, result json.RawMessage, field, key string) []string {
	t.Helper()
	var r map[string][]map[string]any
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range r[field] {
		out = append(out, d[key].(string))
	}
	return out
}

func TestListsBySearchFullAndServer(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "/mcp", call(1, "tools/list", map[string]any{}))
	if got := names(t, r.msg.Result, "tools", "name"); strings.Join(got, ",") != "afs_search,afs_describe,afs_call" {
		t.Errorf("search mode: %v", got)
	}
	e.adm.snap.Mode = "full"
	r = e.do(t, "/mcp", call(2, "tools/list", map[string]any{}))
	want := "github__create_pull_request,github__list_issues,github__search_code,afs_search,afs_describe,afs_call"
	if got := names(t, r.msg.Result, "tools", "name"); strings.Join(got, ",") != want {
		t.Errorf("full mode: %v", got)
	}
	r = e.do(t, "/mcp/github", call(3, "tools/list", map[string]any{}))
	if got := names(t, r.msg.Result, "tools", "name"); strings.Join(got, ",") != "create_pull_request,list_issues,search_code" {
		t.Errorf("per server: %v", got)
	}
	r = e.do(t, "/mcp", call(4, "resources/list", map[string]any{}))
	if got := names(t, r.msg.Result, "resources", "uri"); strings.Join(got, ",") != "wiki__wiki://home" {
		t.Errorf("resources: %v", got)
	}
	r = e.do(t, "/mcp", call(5, "resources/templates/list", map[string]any{}))
	if got := names(t, r.msg.Result, "resourceTemplates", "uriTemplate"); strings.Join(got, ",") != "wiki__wiki://pages/{id}" {
		t.Errorf("templates: %v", got)
	}
	r = e.do(t, "/mcp/wiki", call(6, "prompts/list", map[string]any{}))
	if got := names(t, r.msg.Result, "prompts", "name"); strings.Join(got, ",") != "summarize" {
		t.Errorf("prompts: %v", got)
	}
	if r := e.do(t, "/mcp/jira", call(7, "tools/list", nil)); r.status != 404 || r.raw != "" {
		t.Errorf("server outside the set: %d %q", r.status, r.raw)
	}
}

func TestPagination(t *testing.T) {
	e := newEnv(t)
	e.adm.snap.Mode = "full"
	for i := 0; i < 150; i++ {
		e.adm.snap.Entries = append(e.adm.snap.Entries, allowed("github", "mcp_gh", mcp.KindTool, "tool_"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26)), ""))
	}
	r := e.do(t, "/mcp/github", call(1, "tools/list", map[string]any{}))
	var page struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor string            `json:"nextCursor"`
	}
	_ = json.Unmarshal(r.msg.Result, &page)
	if len(page.Tools) != 100 || page.NextCursor == "" {
		t.Fatalf("page 1: %d %q", len(page.Tools), page.NextCursor)
	}
	r = e.do(t, "/mcp/github", call(2, "tools/list", map[string]any{"cursor": page.NextCursor}))
	var page2 struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor string            `json:"nextCursor"`
	}
	_ = json.Unmarshal(r.msg.Result, &page2)
	if len(page2.Tools) != 53 || page2.NextCursor != "" {
		t.Errorf("page 2: %d %q", len(page2.Tools), page2.NextCursor)
	}
	if r := e.do(t, "/mcp/github", call(3, "prompts/list", map[string]any{"cursor": page.NextCursor})); r.msg.Error == nil {
		t.Error("cursor of another list must be refused")
	}
	if r := e.do(t, "/mcp/github", call(4, "tools/list", map[string]any{"cursor": "junk"})); r.msg.Error == nil {
		t.Error("junk cursor must be refused")
	}
}

func TestCallsAndRefusals(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "/mcp", call(1, "tools/call", map[string]any{"name": "github__create_pull_request", "arguments": map[string]any{"a": 1},
		"_meta": map[string]any{"traceparent": "00-x", "secret": "drop"}}))
	if r.status != 200 || r.msg.Error != nil || !strings.Contains(string(r.msg.Result), "io.afs/requestId") {
		t.Fatalf("call: %d %s", r.status, r.raw)
	}
	up := e.up.calls[len(e.up.calls)-1]
	if !strings.Contains(string(up.Params), `"name":"create_pull_request"`) || strings.Contains(string(up.Params), "secret") || !strings.Contains(string(up.Params), "traceparent") {
		t.Errorf("upstream params: %s", up.Params)
	}
	ev := e.led.last()
	if ev.Outcome != "ok" || ev.ServerID != "mcp_gh" || ev.Name != "create_pull_request" || ev.Digest == "" || ev.Caller.SubjectID != "agt_1" {
		t.Errorf("ledger: %+v", ev)
	}

	for _, tt := range []struct{ path, name, reason string }{
		{"/mcp", "github__delete_repo", "primitive_blocked_team"},
		{"/mcp", "github__nope", "primitive_unknown"},
		{"/mcp", "noserver", "primitive_unknown"},
		{"/mcp/github", "delete_repo", "primitive_blocked_team"},
	} {
		r := e.do(t, tt.path, call(2, "tools/call", map[string]any{"name": tt.name}))
		if r.msg.Error == nil || r.msg.Error.Code != -32602 || !strings.HasPrefix(r.msg.Error.Message, "Unknown tool") {
			t.Errorf("%s: %s", tt.name, r.raw)
		}
		if ev := e.led.last(); ev.Outcome != "refused" || ev.Reason != tt.reason {
			t.Errorf("%s ledger: %+v", tt.name, ev)
		}
	}

	if r := e.do(t, "/mcp", call(3, "tools/call", map[string]any{"name": "github__search_code"})); r.msg.Error != nil {
		t.Errorf("first limited call: %s", r.raw)
	}
	if r := e.do(t, "/mcp", call(4, "tools/call", map[string]any{"name": "github__search_code"})); r.status != 429 || !strings.Contains(r.raw, "rate_limited") {
		t.Errorf("second limited call: %d %s", r.status, r.raw)
	}

	r = e.do(t, "/mcp", call(5, "resources/read", map[string]any{"uri": "wiki__wiki://pages/42"}))
	if r.msg.Error != nil || !strings.Contains(string(e.up.calls[len(e.up.calls)-1].Params), `"uri":"wiki://pages/42"`) {
		t.Errorf("template read: %s", r.raw)
	}
	r = e.do(t, "/mcp/wiki", call(6, "prompts/get", map[string]any{"name": "summarize"}))
	if r.msg.Error != nil {
		t.Errorf("prompt: %s", r.raw)
	}
	for _, m := range []string{"tools/call", "resources/read", "prompts/get"} {
		if r := e.do(t, "/mcp", call(7, m, map[string]any{})); r.msg.Error == nil || r.msg.Error.Code != -32602 {
			t.Errorf("%s without a name: %s", m, r.raw)
		}
	}
}

func TestUpstreamFailures(t *testing.T) {
	cases := []struct {
		name   string
		fn     func(mcpgateway.Server, *mcpwire.Message) (*mcpwire.Message, error)
		status int
		out    string
		reason string
	}{
		{"tool error", func(_ mcpgateway.Server, m *mcpwire.Message) (*mcpwire.Message, error) {
			return mcpwire.NewResult(m.ID, map[string]any{"isError": true, "content": []any{}})
		}, 200, "tool_error", ""},
		{"jsonrpc error", func(_ mcpgateway.Server, m *mcpwire.Message) (*mcpwire.Message, error) {
			return mcpwire.NewError(m.ID, -32603, "boom", nil), nil
		}, 200, "protocol_error", ""},
		{"down", func(mcpgateway.Server, *mcpwire.Message) (*mcpwire.Message, error) { return nil, errors.New("dial") }, 502, "upstream_unavailable", "upstream_unavailable"},
		{"too large", func(mcpgateway.Server, *mcpwire.Message) (*mcpwire.Message, error) {
			return nil, mcpwire.ErrResponseTooLarge
		}, 200, "upstream_unavailable", "response_too_large"},
		{"timeout", func(mcpgateway.Server, *mcpwire.Message) (*mcpwire.Message, error) {
			time.Sleep(300 * time.Millisecond)
			return nil, context.DeadlineExceeded
		}, 504, "timeout", "timeout"},
		{"cancelled", func(mcpgateway.Server, *mcpwire.Message) (*mcpwire.Message, error) { return nil, context.Canceled }, 200, "cancelled", ""},
		{"reconnect", func(mcpgateway.Server, *mcpwire.Message) (*mcpwire.Message, error) {
			return nil, &mcpgateway.UpstreamError{Status: 200, Reason: "auth_reconnect_needed", Message: "reconnect"}
		}, 200, "refused", "auth_reconnect_needed"},
	}
	for _, tt := range cases {
		e := newEnv(t)
		e.up.fn = tt.fn
		r := e.do(t, "/mcp", call(1, "tools/call", map[string]any{"name": "github__list_issues"}))
		ev := e.led.last()
		if r.status != tt.status || ev.Outcome != tt.out || ev.Reason != tt.reason {
			t.Errorf("%s: %d %+v %s", tt.name, r.status, ev, r.raw)
		}
	}
	if (&mcpgateway.UpstreamError{Reason: "x"}).Error() == "" {
		t.Error("Error()")
	}
}

func TestMetaTools(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "/mcp", call(1, "tools/call", map[string]any{"name": "afs_search", "arguments": map[string]any{"query": "pull request"}}))
	var res struct {
		StructuredContent struct {
			Results []struct {
				Name    string `json:"name"`
				Summary string `json:"summary"`
			} `json:"results"`
		} `json:"structuredContent"`
	}
	_ = json.Unmarshal(r.msg.Result, &res)
	if len(res.StructuredContent.Results) == 0 || res.StructuredContent.Results[0].Name != "github__create_pull_request" ||
		res.StructuredContent.Results[0].Summary != "Open a pull request." {
		t.Fatalf("search: %s", r.raw)
	}
	for _, x := range res.StructuredContent.Results {
		if x.Name == "github__delete_repo" {
			t.Error("blocked tool in search results")
		}
	}
	if ev := e.led.last(); ev.Name != "afs_search" || ev.SearchHit == nil || !*ev.SearchHit || ev.QueryText != "pull request" {
		t.Errorf("search ledger: %+v", ev)
	}
	r = e.do(t, "/mcp", call(2, "tools/call", map[string]any{"name": "afs_search", "arguments": map[string]any{"query": "page", "kinds": []string{"prompt"}, "limit": 99}}))
	if !strings.Contains(string(r.msg.Result), "wiki__summarize") || strings.Contains(string(r.msg.Result), "github__") {
		t.Errorf("search by kind: %s", r.raw)
	}
	for _, bad := range []map[string]any{{}, {"query": " "}, {"query": "x", "kinds": []string{"sampling"}}} {
		if r := e.do(t, "/mcp", call(3, "tools/call", map[string]any{"name": "afs_search", "arguments": bad})); r.msg.Error == nil {
			t.Errorf("bad search %v accepted", bad)
		}
	}

	r = e.do(t, "/mcp", call(4, "tools/call", map[string]any{"name": "afs_describe", "arguments": map[string]any{"names": []string{"github__list_issues", "github__delete_repo", "wiki__wiki://pages/{id}", "x"}}}))
	var d struct {
		StructuredContent struct {
			Definitions []map[string]any `json:"definitions"`
			NotFound    []string         `json:"notFound"`
		} `json:"structuredContent"`
	}
	_ = json.Unmarshal(r.msg.Result, &d)
	if len(d.StructuredContent.Definitions) != 2 || len(d.StructuredContent.NotFound) != 2 || d.StructuredContent.Definitions[0]["inputSchema"] == nil {
		t.Errorf("describe: %s", r.raw)
	}
	if r := e.do(t, "/mcp", call(5, "tools/call", map[string]any{"name": "afs_describe", "arguments": map[string]any{"names": []string{}}})); r.msg.Error == nil {
		t.Error("describe with no names")
	}

	for _, tt := range []struct{ name, method string }{
		{"github__list_issues", "tools/call"}, {"wiki__summarize", "prompts/get"}, {"wiki__wiki://home", "resources/read"},
	} {
		r := e.do(t, "/mcp", call(6, "tools/call", map[string]any{"name": "afs_call", "arguments": map[string]any{"name": tt.name}}))
		if r.msg.Error != nil || e.up.calls[len(e.up.calls)-1].Method != tt.method {
			t.Errorf("afs_call %s: %s", tt.name, r.raw)
		}
	}
	if r := e.do(t, "/mcp", call(7, "tools/call", map[string]any{"name": "afs_call", "arguments": map[string]any{"name": "github__delete_repo"}})); r.msg.Error == nil {
		t.Error("afs_call of a blocked tool")
	}
	if r := e.do(t, "/mcp", call(8, "tools/call", map[string]any{"name": "afs_call", "arguments": map[string]any{"name": "zzz"}})); r.msg.Error == nil {
		t.Error("afs_call of an unknown name")
	}
	if r := e.do(t, "/mcp", call(9, "tools/call", map[string]any{"name": "afs_call", "arguments": map[string]any{}})); r.msg.Error == nil {
		t.Error("afs_call without a name")
	}
	if r := e.do(t, "/mcp/github", call(10, "tools/call", map[string]any{"name": "afs_search", "arguments": map[string]any{"query": "x"}})); r.msg.Error == nil {
		t.Error("meta-tools are not on per-server endpoints")
	}
}

func TestProtocolAndSessions(t *testing.T) {
	e := newEnv(t)
	r := e.do(t, "/mcp", call(1, "server/discover", map[string]any{}))
	if !strings.Contains(string(r.msg.Result), "afs_search") || !strings.Contains(string(r.msg.Result), "2026-07-28") {
		t.Errorf("discover: %s", r.raw)
	}
	if r := e.do(t, "/mcp", call(2, "ping", nil)); r.msg.Error != nil {
		t.Errorf("ping: %s", r.raw)
	}
	if r := e.do(t, "/mcp", call(3, "sampling/createMessage", nil)); r.msg.Error == nil || r.msg.Error.Code != -32601 {
		t.Errorf("unknown method: %s", r.raw)
	}
	// 2025-11-25: initialize issues a sealed session that later requests must carry.
	r = e.do(t, "/mcp", call(4, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}}), "MCP-Protocol-Version", "", "Mcp-Method", "")
	sid := r.header.Get("Mcp-Session-Id")
	if sid == "" || !strings.Contains(string(r.msg.Result), `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("initialize: %s", r.raw)
	}
	if r := e.do(t, "/mcp", call(5, "tools/list", nil), "MCP-Protocol-Version", "2025-11-25", "Mcp-Session-Id", sid); r.msg.Error != nil {
		t.Errorf("with session: %s", r.raw)
	}
	if r := e.do(t, "/mcp", call(6, "tools/list", nil), "MCP-Protocol-Version", "2025-11-25"); r.status != 400 {
		t.Errorf("no session: %d", r.status)
	}
	if r := e.do(t, "/mcp", call(7, "tools/list", nil), "MCP-Protocol-Version", "2025-11-25", "Mcp-Session-Id", "bogus"); r.status != 404 {
		t.Errorf("bad session: %d", r.status)
	}
	if r := e.do(t, "/mcp", call(8, "tools/list", nil), "MCP-Protocol-Version", "2024-11-05"); r.msg.Error == nil || !strings.Contains(r.raw, "supported") {
		t.Errorf("old version: %s", r.raw)
	}
	if r := e.do(t, "/mcp", call(9, "tools/call", map[string]any{"name": "a"}), "Mcp-Name", "b"); r.status != 400 || r.msg.Error.Code != -32020 {
		t.Errorf("header mismatch: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "/mcp", `{`); r.status != 400 {
		t.Errorf("parse: %d", r.status)
	}
	if r := e.do(t, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.status != 202 {
		t.Errorf("notification: %d", r.status)
	}
	if r := e.do(t, "/mcp", call(10, "tools/list", nil), "Origin", "https://evil.example"); r.status != 403 {
		t.Errorf("origin: %d", r.status)
	}
	if r := e.do(t, "/mcp", call(11, "tools/list", nil), "X-Test-Unknown", "1"); r.status != 403 {
		t.Errorf("unknown caller: %d", r.status)
	}
	big := call(12, "tools/call", map[string]any{"name": "github__list_issues", "arguments": map[string]string{"x": strings.Repeat("a", 2<<20)}})
	if r := e.do(t, "/mcp", big); r.status != 413 {
		t.Errorf("too large: %d", r.status)
	}
	e.adm.err = errors.New("db down")
	if r := e.do(t, "/mcp", call(13, "tools/list", nil)); r.status != 503 {
		t.Errorf("admission down must fail closed: %d", r.status)
	}
	for path, want := range map[string]int{"/other": 404, "/mcp/AFS!": 404} {
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil))
		if w.Code != want {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	for method, want := range map[string]int{http.MethodGet: 405, http.MethodDelete: 204} {
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), method, "/mcp", nil))
		if w.Code != want {
			t.Errorf("%s: %d", method, w.Code)
		}
	}
}

func TestNewAndSummary(t *testing.T) {
	if _, err := mcpgateway.New(mcpgateway.Config{}); err == nil {
		t.Error("New without ports")
	}
	if got := mcpgateway.Summary("One. Two."); got != "One." {
		t.Errorf("Summary %q", got)
	}
	if got := mcpgateway.Summary(strings.Repeat("a", 300)); len([]rune(got)) != 200 {
		t.Errorf("Summary length %d", len([]rune(got)))
	}
	_ = io.Discard
}
