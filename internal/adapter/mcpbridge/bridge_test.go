package mcpbridge_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpbridge"
)

// TestMain doubles as the stdio fixture server when AFS_BRIDGE_FIXTURE is set.
func TestMain(m *testing.M) {
	if os.Getenv("AFS_BRIDGE_FIXTURE") == "1" {
		fixture()
		return
	}
	os.Exit(m.Run())
}

func fixture() {
	out := bufio.NewWriter(os.Stdout)
	send := func(s string) { _, _ = fmt.Fprintln(out, s); _ = out.Flush() }
	_, _ = fmt.Fprintln(os.Stderr, "fixture starting")
	send("this is not json")
	send(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`)
	initialized := false
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(sc.Bytes(), &m)
		switch {
		case m.Method == "initialize":
			initialized = true
			send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"protocolVersion":"2025-11-25"}}`)
		case m.Method == "notifications/initialized", m.Method == "notifications/cancelled":
		case m.Method == "tools/list":
			send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[],"initialized":%v}}`, m.ID, initialized))
		case m.Method == "tools/call" && m.Params.Name == "crash":
			os.Exit(3)
		case m.Method == "tools/call" && m.Params.Name == "slow":
			time.Sleep(2 * time.Second)
		case m.Method == "tools/call" && m.Params.Name == "ask":
			send(`{"jsonrpc":"2.0","id":"srv-1","method":"sampling/createMessage","params":{}}`)
			// wait for the bridge's refusal, then answer
			if sc.Scan() && strings.Contains(sc.Text(), "-32601") {
				send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"refused":true}}`)
			}
		case m.Method == "tools/call":
			send(`{"jsonrpc":"2.0","id":"999","result":{}}`) // stray response, ignored
			send(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"content":[{"type":"text","text":"` + m.Params.Name + `"}]}}`)
		}
	}
}

func start(t *testing.T, timeout time.Duration) (*mcpbridge.Bridge, *httptest.Server) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := mcpbridge.New(mcpbridge.Config{Command: []string{exe}, Env: []string{"AFS_BRIDGE_FIXTURE=1"}, Timeout: timeout, Backoff: 10 * time.Millisecond, MaxRequest: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = b.Run(ctx); close(done) }()
	<-b.Ready()
	srv := httptest.NewServer(b)
	t.Cleanup(func() { srv.Close(); cancel(); <-done })
	return b, srv
}

func post(t *testing.T, url, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url+"/mcp", strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func get(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func waitRunning(t *testing.T, b *mcpbridge.Bridge) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !b.Running() {
		if time.Now().After(deadline) {
			t.Fatal("bridge did not restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBridgeCalls(t *testing.T) {
	b, srv := start(t, 5*time.Second)
	waitRunning(t, b)
	code, body := post(t, srv.URL, `{"jsonrpc":"2.0","id":"a-1","method":"initialize","params":{}}`)
	if code != 200 || !strings.Contains(body, `"id":"a-1"`) {
		t.Fatalf("initialize: %d %s", code, body)
	}
	if code, _ := post(t, srv.URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Errorf("notification: %d", code)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("t%d", i)
			code, body := post(t, srv.URL, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"%s"}}`, 7, name),
				"Mcp-Method", "tools/call", "Mcp-Name", name)
			if code != 200 || !strings.Contains(body, `"id":7`) || !strings.Contains(body, name) {
				t.Errorf("call %s: %d %s", name, code, body)
			}
		}(i)
	}
	wg.Wait()
	code, body = post(t, srv.URL, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"ask"}}`)
	if code != 200 || !strings.Contains(body, "refused") {
		t.Errorf("server request: %d %s", code, body)
	}
}

func TestBridgeRestartReplaysInitialize(t *testing.T) {
	b, srv := start(t, 5*time.Second)
	waitRunning(t, b)
	post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	code, body := post(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"crash"}}`)
	if code != 200 || !strings.Contains(body, "server_restarted") || !strings.Contains(body, `"id":2`) {
		t.Fatalf("crash: %d %s", code, body)
	}
	waitRunning(t, b)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body = post(t, srv.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
		if strings.Contains(body, `"initialized":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("initialize not replayed: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBridgeTimeoutAndErrors(t *testing.T) {
	b, srv := start(t, 200*time.Millisecond)
	waitRunning(t, b)
	code, body := post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`)
	if code != 504 || !strings.Contains(body, "-32001") {
		t.Errorf("timeout: %d %s", code, body)
	}
	tests := []struct {
		name, body string
		hdr        []string
		code       int
	}{
		{"parse", `{`, nil, 400},
		{"header mismatch", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, []string{"Mcp-Method", "tools/call"}, 400},
		{"too large", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"` + strings.Repeat("a", 5000) + `"}}`, nil, 413},
		{"response is accepted", `{"jsonrpc":"2.0","id":1,"result":{}}`, nil, 202},
	}
	for _, tt := range tests {
		if code, body := post(t, srv.URL, tt.body, tt.hdr...); code != tt.code {
			t.Errorf("%s: %d %s", tt.name, code, body)
		}
	}
	for path, want := range map[string]int{"/healthz": 200, "/other": 404} {
		resp, err := get(srv.URL + path)
		if err != nil || resp.StatusCode != want {
			t.Errorf("%s: %v %v", path, resp, err)
		}
		_ = resp.Body.Close()
	}
	resp, _ := get(srv.URL + "/mcp")
	if resp.StatusCode != 405 {
		t.Errorf("GET /mcp: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestBridgeNotRunning(t *testing.T) {
	b, err := mcpbridge.New(mcpbridge.Config{Command: []string{"/nonexistent/server"}, Backoff: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = b.Run(ctx); close(done) }()
	srv := httptest.NewServer(b)
	defer func() { srv.Close(); cancel(); <-done }()
	if code, _ := post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != 503 {
		t.Errorf("request: %d", code)
	}
	if code, _ := post(t, srv.URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 503 {
		t.Errorf("notification: %d", code)
	}
	resp, _ := get(srv.URL + "/healthz")
	if resp.StatusCode != 503 {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if _, err := mcpbridge.New(mcpbridge.Config{}); err == nil {
		t.Error("empty command: want error")
	}
}
