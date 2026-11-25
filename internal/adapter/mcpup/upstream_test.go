package mcpup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpgateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpoauth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpinventory"
)

// fakeServer is an upstream MCP server. It records the Authorization header of each request.
type fakeServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	auth     []string
	sessions int
	dropOnce bool
	elicit   bool
	tools    []map[string]any
	pageSize int
	gotReply *mcpwire.Message
}

func newFake(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{pageSize: 1, tools: []map[string]any{
		{"name": "create_issue", "description": "Create an issue.", "inputSchema": map[string]any{"type": "object"}},
		{"name": "list_issues", "description": "List issues.", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	var m mcpwire.Message
	_ = json.NewDecoder(r.Body).Decode(&m)
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Api-Key"))
	f.mu.Unlock()
	if m.IsResponse() {
		f.mu.Lock()
		f.gotReply = &m
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}
	v := r.Header.Get(mcpwire.HeaderProtocolVersion)
	if v == mcpwire.Version20251125 && m.Method != "initialize" && r.Header.Get(mcpwire.HeaderSessionID) != "s-1" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if f.dropOnce && m.Method == "tools/call" {
		f.dropOnce = false
		w.WriteHeader(http.StatusNotFound)
		return
	}
	reply := func(result any) {
		b, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcpwire.Message{JSONRPC: "2.0", ID: m.ID, Result: b})
	}
	switch m.Method {
	case "initialize":
		f.mu.Lock()
		f.sessions++
		f.mu.Unlock()
		w.Header().Set(mcpwire.HeaderSessionID, "s-1")
		reply(map[string]any{"protocolVersion": v, "capabilities": map[string]any{"tools": map[string]any{}}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		if f.elicit {
			w.Header().Set("Content-Type", "text/event-stream")
			req, _ := json.Marshal(mcpwire.Message{JSONRPC: "2.0", ID: json.RawMessage(`"e1"`), Method: "elicitation/create", Params: json.RawMessage(`{}`)})
			res, _ := json.Marshal(mcpwire.Message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{"content":[]}`)})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", req, res)
			return
		}
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}})
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(m.Params, &p)
		i := 0
		if p.Cursor != "" {
			_, _ = fmt.Sscan(p.Cursor, &i)
		}
		end := min(i+f.pageSize, len(f.tools))
		out := map[string]any{"tools": f.tools[i:end]}
		if end < len(f.tools) {
			out["nextCursor"] = fmt.Sprint(end)
		}
		reply(out)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcpwire.NewError(m.ID, mcpwire.CodeMethodNotFound, "no", nil))
	}
}

func (f *fakeServer) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth[len(f.auth)-1]
}

type targets map[string]Target

func (t targets) Target(_ context.Context, id string) (Target, error) {
	x, ok := t[id]
	if !ok {
		return Target{}, errors.New("no target")
	}
	return x, nil
}

type fakeTokens struct {
	refreshErr error
	gotRefresh []string
	rotate     string
	exchanged  string
	calls      int
}

func (f *fakeTokens) Discover(context.Context, string) (mcpcatalog.OAuthServer, error) {
	return mcpcatalog.OAuthServer{TokenEndpoint: "https://as.example/token", Resource: "https://res.example/mcp"}, nil //nolint:gosec // fixture URLs
}

func (f *fakeTokens) Refresh(_ context.Context, ep, client, rt, res string) (mcpoauth.AccessToken, error) {
	f.calls++
	f.gotRefresh = append(f.gotRefresh, rt)
	if f.refreshErr != nil {
		return mcpoauth.AccessToken{}, f.refreshErr
	}
	return mcpoauth.AccessToken{Value: "at-user", Expires: time.Now().Add(time.Hour), Refresh: f.rotate}, nil
}

func (f *fakeTokens) ClientCredentials(_ context.Context, ep, id, secret, res string) (mcpoauth.AccessToken, error) {
	f.calls++
	if id != "cid" || secret != "csec" || res != "https://res.example/mcp" {
		return mcpoauth.AccessToken{}, errors.New("bad client")
	}
	return mcpoauth.AccessToken{Value: "at-client", Expires: time.Now().Add(time.Hour)}, nil
}

func (f *fakeTokens) TokenExchange(_ context.Context, ep, subject, aud, res string) (mcpoauth.AccessToken, error) {
	f.calls++
	f.exchanged = subject
	if subject == "bad" {
		return mcpoauth.AccessToken{}, mcpoauth.ErrInvalidGrant
	}
	return mcpoauth.AccessToken{Value: "at-fed", Expires: time.Now().Add(time.Hour)}, nil
}

type fakeState struct {
	marks, feds []string
}

func (s *fakeState) MarkMcpCredential(_ context.Context, id, scope, status string) error {
	s.marks = append(s.marks, id+"/"+scope+"="+status)
	return nil
}

func (s *fakeState) ReportMcpFederation(_ context.Context, id, team, status, msg string) error {
	s.feds = append(s.feds, id+"/"+team+"="+status)
	return nil
}

type fakeSigner struct{ service, region string }

func (f *fakeSigner) Sign(_ context.Context, req *http.Request, _ []byte, service, region string) error {
	f.service, f.region = service, region
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 signed")
	return nil
}

type rig struct {
	u      *Upstream
	f      *fakeServer
	tok    *fakeTokens
	state  *fakeState
	signer *fakeSigner
	dir    string
	ts     targets
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{f: newFake(t), tok: &fakeTokens{}, state: &fakeState{}, signer: &fakeSigner{}, dir: t.TempDir(), ts: targets{}}
	fed := t.TempDir()
	r.u = New(Config{Targets: r.ts, State: r.state, Tokens: r.tok, Signer: r.signer, TeamID: "team_1", Namespace: "afs-t-a",
		CredentialsDir: r.dir, FederationRoot: fed, Region: "us-west-2",
		HTTP: func(Target) (*http.Client, error) { return r.f.srv.Client(), nil }})
	_ = os.MkdirAll(filepath.Join(fed, domain.McpFederationDir("api://issues")), 0o700)
	_ = os.WriteFile(filepath.Join(fed, domain.McpFederationDir("api://issues"), "token"), []byte("jwt-1\n"), 0o600)
	return r
}

func (r *rig) target(t *testing.T, slug string, kind domain.McpAuthKind, version string) mcpgateway.Server {
	t.Helper()
	r.ts["mcp_"+slug] = Target{ID: "mcp_" + slug, Slug: slug, Transport: domain.McpTransportStreamableHTTP, URL: r.f.srv.URL + "/mcp",
		AuthKind: kind, ProtocolVersion: version, Federation: &domain.McpFederation{Audience: "api://issues", TokenEndpoint: "https://login.example/token"}} //nolint:gosec // fixture URL
	return mcpgateway.Server{ID: "mcp_" + slug, Slug: slug}
}

func (r *rig) secret(t *testing.T, slug, v string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, domain.McpCredentialKey(slug)), []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, u *Upstream, s mcpgateway.Server) (*mcpwire.Message, error) {
	t.Helper()
	return u.Call(t.Context(), s, &mcpwire.Message{JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: "tools/call", Params: json.RawMessage(`{"name":"create_issue"}`)})
}

func TestCredentialsByKind(t *testing.T) {
	r := newRig(t)
	// none, stateless 2026-07-28
	s := r.target(t, "plain", domain.McpAuthNone, mcpwire.Version20260728)
	if resp, err := call(t, r.u, s); err != nil || resp.Error != nil || r.f.lastAuth() != "|" {
		t.Fatalf("none: %+v %v %q", resp, err, r.f.lastAuth())
	}
	// static: bearer, or the named header
	s = r.target(t, "stat", domain.McpAuthStatic, mcpwire.Version20260728)
	if _, err := call(t, r.u, s); !errors.Is(err, ErrCredentialMissing) {
		t.Errorf("static without a mount = %v", err)
	}
	r.secret(t, "stat", "tok-1\n")
	if _, err := call(t, r.u, s); err != nil || r.f.lastAuth() != "Bearer tok-1|" {
		t.Errorf("static bearer: %v %q", err, r.f.lastAuth())
	}
	x := r.ts["mcp_stat"]
	x.AuthHeader = "X-Api-Key"
	r.ts["mcp_stat"] = x
	if _, err := call(t, r.u, s); err != nil || r.f.lastAuth() != "|tok-1" {
		t.Errorf("static header: %v %q", err, r.f.lastAuth())
	}
	// oauth_client: discovery, client credentials, cached
	s = r.target(t, "cc", domain.McpAuthOAuthClient, mcpwire.Version20260728)
	r.secret(t, "cc", `{"clientId":"cid","clientSecret":"csec"}`)
	before := r.tok.calls
	for range 2 {
		if _, err := call(t, r.u, s); err != nil || r.f.lastAuth() != "Bearer at-client|" {
			t.Fatalf("oauth_client: %v %q", err, r.f.lastAuth())
		}
	}
	if r.tok.calls-before != 1 {
		t.Errorf("client credentials minted %d times, want once", r.tok.calls-before)
	}
	// workload_federation: the projected token, exchanged, ready reported
	s = r.target(t, "fed", domain.McpAuthWorkloadFederation, mcpwire.Version20260728)
	if _, err := call(t, r.u, s); err != nil || r.f.lastAuth() != "Bearer at-fed|" || r.tok.exchanged != "jwt-1" {
		t.Errorf("federation: %v %q %q", err, r.f.lastAuth(), r.tok.exchanged)
	}
	if len(r.state.feds) != 1 || r.state.feds[0] != "mcp_fed/team_1=ready" {
		t.Errorf("federation state = %v", r.state.feds)
	}
	// aws_sigv4
	s = r.target(t, "aws", domain.McpAuthAWSSigV4, mcpwire.Version20260728)
	if _, err := call(t, r.u, s); err != nil || r.f.lastAuth() != "AWS4-HMAC-SHA256 signed|" || r.signer.region != "us-west-2" {
		t.Errorf("sigv4: %v %q %+v", err, r.f.lastAuth(), r.signer)
	}
	// unknown kind
	s = r.target(t, "odd", domain.McpAuthKind("odd"), mcpwire.Version20260728)
	if _, err := call(t, r.u, s); !errors.Is(err, ErrCredentialMissing) {
		t.Errorf("unknown kind = %v", err)
	}
	if _, err := r.u.Call(t.Context(), mcpgateway.Server{ID: "gone"}, &mcpwire.Message{}); err == nil {
		t.Error("an unknown server was called")
	}
}

func TestOAuthUserRefresh(t *testing.T) {
	r := newRig(t)
	s := r.target(t, "saas", domain.McpAuthOAuthUser, mcpwire.Version20260728)
	var ue *mcpgateway.UpstreamError
	if _, err := call(t, r.u, s); !errors.As(err, &ue) || ue.Reason != "auth_reconnect_needed" {
		t.Fatalf("no secret = %v", err)
	}
	r.secret(t, "saas", `{"refreshToken":"rt-1","tokenEndpoint":"https://as.example/token","clientId":"https://afs.example/c.json","resource":"https://res.example/mcp"}`)
	r.tok.rotate = "rt-2"
	if _, err := call(t, r.u, s); err != nil || r.f.lastAuth() != "Bearer at-user|" {
		t.Fatalf("refresh: %v %q", err, r.f.lastAuth())
	}
	if len(r.state.marks) != 1 || r.state.marks[0] != "mcp_saas/team_1=set" {
		t.Errorf("marks = %v", r.state.marks)
	}
	// A refused access token is dropped; the next mint uses the rotated refresh token.
	delete(r.u.tokens, "mcp_saas")
	r.tok.refreshErr = errors.Join(errors.New("x"), mcpoauth.ErrInvalidGrant)
	if _, err := call(t, r.u, s); !errors.As(err, &ue) || ue.Reason != "auth_reconnect_needed" {
		t.Fatalf("invalid grant = %v", err)
	}
	if got := r.tok.gotRefresh[len(r.tok.gotRefresh)-1]; got != "rt-2" {
		t.Errorf("refreshed with %q, want the rotated token", got)
	}
	if r.state.marks[len(r.state.marks)-1] != "mcp_saas/team_1=expired" {
		t.Errorf("marks = %v", r.state.marks)
	}
	r.tok.refreshErr = errors.New("network")
	if _, err := call(t, r.u, s); err == nil || errors.As(err, &ue) {
		t.Errorf("an unreachable token endpoint = %v", err)
	}
}

func TestFederationNotReady(t *testing.T) {
	r := newRig(t)
	s := r.target(t, "fed", domain.McpAuthWorkloadFederation, mcpwire.Version20260728)
	x := r.ts["mcp_fed"]
	x.Federation = &domain.McpFederation{Audience: "api://other", TokenEndpoint: "https://login.example/token"} //nolint:gosec // fixture URL
	r.ts["mcp_fed"] = x
	var ue *mcpgateway.UpstreamError
	if _, err := call(t, r.u, s); !errors.As(err, &ue) || ue.Reason != "federation_not_ready" || ue.Status != http.StatusServiceUnavailable {
		t.Fatalf("no projected token = %v", err)
	}
	x.Federation = nil
	r.ts["mcp_fed"] = x
	if _, err := call(t, r.u, s); !errors.As(err, &ue) {
		t.Errorf("no federation = %v", err)
	}
	r.u.cfg.Signer = nil
	s = r.target(t, "aws", domain.McpAuthAWSSigV4, "")
	if _, err := call(t, r.u, s); !errors.As(err, &ue) || ue.Reason != "federation_not_ready" {
		t.Errorf("no signer = %v", err)
	}
	if r.state.feds[0] != "mcp_fed/team_1=failing" {
		t.Errorf("federation state = %v", r.state.feds)
	}
}

func TestSessionsAndElicitation(t *testing.T) {
	r := newRig(t)
	s := r.target(t, "old", domain.McpAuthNone, "")
	if resp, err := call(t, r.u, s); err != nil || resp.Error != nil {
		t.Fatalf("2025-11-25 call: %+v %v", resp, err)
	}
	if _, err := call(t, r.u, s); err != nil || r.f.sessions != 1 {
		t.Fatalf("the session was not reused: %d %v", r.f.sessions, err)
	}
	r.f.dropOnce = true
	if _, err := call(t, r.u, s); err != nil || r.f.sessions != 2 {
		t.Fatalf("a dropped session was not re-initialised: %d %v", r.f.sessions, err)
	}
	r.f.elicit = true
	resp, err := call(t, r.u, s)
	if err != nil || !strings.Contains(string(resp.Result), `"isError":true`) || !strings.Contains(string(resp.Result), "elicitation_unsupported") {
		t.Fatalf("elicitation: %+v %v", resp, err)
	}
	if r.f.gotReply == nil || !strings.Contains(string(r.f.gotReply.Result), "decline") {
		t.Errorf("the server's request was not declined: %+v", r.f.gotReply)
	}
}

func TestLister(t *testing.T) {
	r := newRig(t)
	r.target(t, "inv", domain.McpAuthNone, mcpwire.Version20260728)
	l, err := r.u.Lister(t.Context(), r.ts["mcp_inv"])
	if err != nil {
		t.Fatal(err)
	}
	listed, truncated, err := mcpinventory.Fetch(t.Context(), l)
	if err != nil || truncated || len(listed["tool"]) != 2 {
		t.Fatalf("fetch = %v %v %v", listed, truncated, err)
	}
	if _, err := r.u.Lister(t.Context(), Target{ID: "x", Slug: "x", AuthKind: domain.McpAuthStatic}); err == nil {
		t.Error("a lister without its credential")
	}
}

func TestHelpers(t *testing.T) {
	for raw, want := range map[string][2]string{
		"https://bedrock-agentcore.us-east-1.amazonaws.com/mcp": {"bedrock-agentcore", "us-east-1"},
		"https://abc.execute-api.eu-west-1.amazonaws.com/x":     {"execute-api", "eu-west-1"},
		"https://mcp.corp.example/mcp":                          {"execute-api", "us-west-2"},
	} {
		if s, r := sigv4Scope(raw, "us-west-2"); s != want[0] || r != want[1] {
			t.Errorf("sigv4Scope(%s) = %s %s", raw, s, r)
		}
	}
	u := New(Config{Namespace: "afs-t-a"})
	if got := u.URL(Target{Transport: domain.McpTransportHosted, Slug: "files"}); got != "http://afs-mcp-files.afs-t-a.svc:8080/mcp" {
		t.Errorf("hosted URL = %s", got)
	}
	if c, err := u.httpClient(Target{ID: "a", CABundle: "not pem"}); err == nil || c != nil {
		t.Error("a bad CA bundle was accepted")
	}
	c1, _ := u.httpClient(Target{ID: "a"})
	c2, _ := u.httpClient(Target{ID: "a"})
	if c1 == nil || c1 != c2 {
		t.Error("the client is not reused")
	}
	if u.scope(Target{TeamID: "team_x"}) != "team_x" || u.scope(Target{CredentialMode: domain.McpCredentialShared}) != domain.McpSharedScope {
		t.Error("scope")
	}
}

func TestCachedTargets(t *testing.T) {
	n := 0
	now := time.Unix(0, 0)
	c := &CachedTargets{Next: countingTargets{&n}, Now: func() time.Time { return now }}
	for range 3 {
		if _, err := c.Target(t.Context(), "a"); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Minute)
	_, _ = c.Target(t.Context(), "a")
	c.InvalidateAll()
	_, _ = c.Target(t.Context(), "a")
	if n != 3 {
		t.Errorf("reads = %d, want 3", n)
	}
	if _, err := c.Target(t.Context(), "fail"); err == nil {
		t.Error("an error was cached as a target")
	}
}

type countingTargets struct{ n *int }

func (c countingTargets) Target(_ context.Context, id string) (Target, error) {
	if id == "fail" {
		return Target{}, errors.New("x")
	}
	*c.n++
	return Target{ID: id}, nil
}
