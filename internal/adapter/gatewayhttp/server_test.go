package gatewayhttp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock/fakebedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp/gwtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

const (
	converseBody = `{"messages":[{"role":"user","content":[{"text":"hi"}]}],"inferenceConfig":{"maxTokens":100000},"requestMetadata":{"team":"someone-elses"}}`
	anthropicReq = `{"model":"` + gwtest.Model + `","max_tokens":100000,"stream":%s,"messages":[{"role":"user","content":"hi"}]}`
	invokeBody   = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`
)

func post(t *testing.T, r *gwtest.Rig, path, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.HTTP.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func errBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readAll(t, resp), &m); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	return m
}

func modelPath(op string) string { return "/model/" + gwtest.Model + "/" + op }

func TestCompleteCalls(t *testing.T) {
	cases := map[string]struct {
		path, body string
		op         fakebedrock.Op
		usageKey   string
	}{
		"converse":  {modelPath("converse"), converseBody, fakebedrock.OpConverse, "usage"},
		"invoke":    {modelPath("invoke"), invokeBody, fakebedrock.OpInvoke, "usage"},
		"anthropic": {"/anthropic/v1/messages", strings.Replace(anthropicReq, "%s", "false", 1), fakebedrock.OpInvoke, "usage"},
	}
	role, err := gateway.TeamRoleARN("aws", "111122223333", "afs-t-alpha", gwtest.TeamA)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := gwtest.New(t)
			r.Fake.SetUsage(fakebedrock.Usage{Input: 11, Output: 7})
			resp := post(t, r, c.path, c.body, map[string]string{
				"Authorization": "AWS4-HMAC-SHA256 Credential=AKIAIGNOREDIGNORED/20261122/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=00",
				"X-Api-Key":     "sk-ant-ignored",
			})
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
			}
			if got := resp.Header.Get(gatewayhttp.VersionHeader); got != "1.0.0" {
				t.Errorf("version header %q", got)
			}
			var doc map[string]any
			if err := json.Unmarshal(readAll(t, resp), &doc); err != nil {
				t.Fatal(err)
			}
			if _, ok := doc[c.usageKey]; !ok {
				t.Errorf("response lacks %s: %v", c.usageKey, doc)
			}
			r.Svc.Wait()
			calls := r.Fake.Calls()
			if len(calls) != 1 {
				t.Fatalf("upstream calls %d", len(calls))
			}
			up := calls[0]
			if up.Op != c.op {
				t.Errorf("op %s", up.Op)
			}
			if up.AccessKeyID != gwtest.KeyFor(role) {
				t.Errorf("signed with %q, not the team role's credentials", up.AccessKeyID)
			}
			if strings.Contains(up.Header.Get("Authorization"), "AKIAIGNORED") || up.Header.Get("X-Api-Key") != "" {
				t.Error("the agent's own credentials reached Bedrock")
			}
			if !strings.Contains(up.ModelID, "application-inference-profile/aaa") {
				t.Errorf("upstream model %q is not the team's profile", up.ModelID)
			}
			var ub map[string]any
			_ = json.Unmarshal(up.Body, &ub)
			if name == "converse" {
				md, _ := ub["requestMetadata"].(map[string]any)
				if md["team"] == "someone-elses" || md["afs.team"] == nil && md["afs_team_id"] == nil && len(md) == 0 {
					t.Errorf("requestMetadata not replaced: %v", md)
				}
				ic, _ := ub["inferenceConfig"].(map[string]any)
				if ic["maxTokens"] != float64(4096) {
					t.Errorf("maxTokens %v, want capped to 4096", ic["maxTokens"])
				}
			} else if ub["max_tokens"] != float64(4096) {
				t.Errorf("max_tokens %v, want 4096", ub["max_tokens"])
			}
			if name == "anthropic" {
				if _, ok := ub["model"]; ok {
					t.Error("model must not be in the Bedrock body")
				}
				if ub["anthropic_version"] != "bedrock-2023-05-31" {
					t.Errorf("anthropic_version %v", ub["anthropic_version"])
				}
			}
			rows := r.Usage.All()
			if len(rows) != 1 || rows[0].Outcome != gateway.OutcomeOK || rows[0].Usage.Input != 11 || rows[0].Usage.Output != 7 || rows[0].AgentID != "agt_01" {
				t.Errorf("usage rows %+v", rows)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", 21<<20) + `"}`
	cases := map[string]struct {
		setup  func(*gwtest.Rig)
		path   string
		body   string
		status int
		code   string
		hdr    string
	}{
		"bad json":          {nil, modelPath("converse"), `[1]`, 400, "ValidationError", ""},
		"bad model id":      {nil, "/model/not%20valid/converse", `{}`, 400, "ValidationError", ""},
		"missing max":       {nil, "/anthropic/v1/messages", `{"model":"` + gwtest.Model + `"}`, 400, "ValidationError", ""},
		"too large":         {nil, modelPath("converse"), big, 413, "ValidationError", ""},
		"unknown path":      {nil, "/model/x/frobnicate", `{}`, 404, "NotFound", ""},
		"model not enabled": {func(r *gwtest.Rig) { delete(r.Store.Models, gwtest.TeamA+"\x00"+gwtest.Model) }, modelPath("converse"), converseBody, 403, "AccessDenied", ""},
		"suspended": {func(r *gwtest.Rig) {
			tm := r.Store.Teams[gwtest.TeamA]
			tm.State = "suspended"
			r.Store.Teams[gwtest.TeamA] = tm
		}, modelPath("converse"), converseBody, 403, "AccessDenied", ""},
		"budget exhausted": {func(r *gwtest.Rig) { r.Spend.Limit = domain.MustMoney("0.000001") }, modelPath("converse"), converseBody, 402, "BudgetExceeded", ""},
		"throttled":        {func(r *gwtest.Rig) { r.Fake.SetMode(fakebedrock.ModeThrottle) }, modelPath("converse"), converseBody, 429, "Throttled", "Retry-After"},
		"upstream error":   {func(r *gwtest.Rig) { r.Fake.SetMode(fakebedrock.ModeServerError) }, modelPath("converse"), converseBody, 502, "UpstreamError", ""},
		"upstream 503":     {func(r *gwtest.Rig) { r.Fake.SetMode(fakebedrock.ModeUnavailable) }, modelPath("converse"), converseBody, 502, "UpstreamError", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := gwtest.New(t, func(r *gwtest.Rig) {
				if c.setup != nil {
					c.setup(r)
				}
			})
			resp := post(t, r, c.path, c.body, nil)
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, c.status, readAll(t, resp))
			}
			if resp.Header.Get(gatewayhttp.VersionHeader) != "1.0.0" {
				t.Error("version header missing on an error")
			}
			b := errBody(t, resp)
			if b["code"] != c.code || b["requestId"] == "" || b["message"] == "" {
				t.Errorf("body %v", b)
			}
			if c.hdr != "" && resp.Header.Get(c.hdr) == "" {
				t.Errorf("missing header %s", c.hdr)
			}
			if name == "suspended" {
				if d, _ := b["details"].(map[string]any); d["reason"] != "team_suspended" {
					t.Errorf("details %v", b["details"])
				}
			}
			if name == "budget exhausted" {
				d, _ := b["details"].(map[string]any)
				if d["budgetId"] != gwtest.BudgetA {
					t.Errorf("details %v", b["details"])
				}
			}
		})
	}
}

func TestAnthropicErrorShape(t *testing.T) {
	r := gwtest.New(t, func(r *gwtest.Rig) { r.Spend.Limit = domain.MustMoney("0.000001") })
	resp := post(t, r, "/anthropic/v1/messages", strings.Replace(anthropicReq, "%s", "false", 1), nil)
	if resp.StatusCode != 402 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	b := errBody(t, resp)
	e, _ := b["error"].(map[string]any)
	if b["type"] != "error" || e["type"] != "permission_error" || e["code"] != "BudgetExceeded" || b["code"] != "BudgetExceeded" {
		t.Errorf("body %v", b)
	}
}

func TestUnknownCallerIsUnauthenticatedAndNotMetered(t *testing.T) {
	r := gwtest.New(t)
	for name, addr := range map[string]string{"unknown ip": "10.9.9.9:4000", "garbage": "nonsense"} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, modelPath("converse"), strings.NewReader(converseBody))
			req.RemoteAddr = addr
			req.Header.Set("X-Forwarded-For", "127.0.0.1")
			rec := httptest.NewRecorder()
			r.Server.Handler().ServeHTTP(rec, req)
			if rec.Code != 401 || rec.Header().Get(gatewayhttp.VersionHeader) != "1.0.0" {
				t.Fatalf("status %d headers %v", rec.Code, rec.Header())
			}
		})
	}
	if len(r.Fake.Calls()) != 0 || len(r.Usage.All()) != 0 || len(r.Spend.Reservs) != 0 {
		t.Error("an unauthenticated call reached Bedrock, spend or usage")
	}
}

func TestHealthz(t *testing.T) {
	r := gwtest.New(t)
	resp, err := http.Get(r.HTTP.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get(gatewayhttp.VersionHeader) != "1.0.0" {
		t.Errorf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
	if p := post(t, r, "/healthz", "", nil); p.StatusCode != 404 {
		t.Errorf("POST /healthz = %d", p.StatusCode)
	}
}

// ---- streaming ----

type frame struct {
	kind, typ string // event|exception, event or exception type
	payload   []byte
}

func readFrames(t *testing.T, body io.Reader) []frame {
	t.Helper()
	dec := eventstream.NewDecoder()
	var out []frame
	for {
		m, err := dec.Decode(body, nil)
		if err != nil {
			if err == io.EOF {
				return out
			}
			t.Fatalf("decode: %v", err)
		}
		f := frame{kind: m.Headers.Get(":message-type").String(), payload: m.Payload}
		if f.kind == "exception" {
			f.typ = m.Headers.Get(":exception-type").String()
		} else {
			f.typ = m.Headers.Get(":event-type").String()
		}
		out = append(out, f)
	}
}

func TestConverseStream(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 20, Output: 9})
	resp := post(t, r, modelPath("converse-stream"), converseBody, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/vnd.amazon.eventstream" {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	fs := readFrames(t, resp.Body)
	if len(fs) < 4 || fs[0].typ != "messageStart" || fs[len(fs)-1].typ != "metadata" {
		t.Fatalf("frames %v", fs)
	}
	r.Svc.Wait()
	rows := r.Usage.All()
	if len(rows) != 1 || rows[0].Usage.Input != 20 || rows[0].Usage.Output != 9 || rows[0].Outcome != gateway.OutcomeOK {
		t.Errorf("rows %+v", rows)
	}
}

func sse(t *testing.T, body io.Reader) (events []string, data []map[string]any) {
	t.Helper()
	raw, _ := io.ReadAll(body)
	for _, blk := range strings.Split(strings.TrimSpace(string(raw)), "\n\n") {
		var ev string
		var d map[string]any
		for _, ln := range strings.Split(blk, "\n") {
			if v, ok := strings.CutPrefix(ln, "event: "); ok {
				ev = v
			}
			if v, ok := strings.CutPrefix(ln, "data: "); ok {
				if err := json.Unmarshal([]byte(v), &d); err != nil {
					t.Fatalf("bad sse data %q", v)
				}
			}
		}
		events, data = append(events, ev), append(data, d)
	}
	return
}

func TestAnthropicStream(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 20, Output: 9})
	resp := post(t, r, "/anthropic/v1/messages", strings.Replace(anthropicReq, "%s", "true", 1), nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ev, data := sse(t, resp.Body)
	if ev[0] != "message_start" || ev[len(ev)-1] != "message_stop" || data[0]["type"] != "message_start" {
		t.Fatalf("events %v", ev)
	}
	r.Svc.Wait()
	rows := r.Usage.All()
	if len(rows) != 1 || rows[0].Usage.Input != 20 || rows[0].Usage.Output != 9 {
		t.Errorf("rows %+v", rows)
	}
}

func TestInvokeStreamPassesChunks(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 5, Output: 3})
	resp := post(t, r, modelPath("invoke-with-response-stream"), invokeBody, nil)
	fs := readFrames(t, resp.Body)
	var first map[string]any
	var c struct{ Bytes string }
	_ = json.Unmarshal(fs[0].payload, &c)
	raw, _ := base64.StdEncoding.DecodeString(c.Bytes)
	_ = json.Unmarshal(raw, &first)
	if fs[0].typ != "chunk" || first["type"] != "message_start" {
		t.Fatalf("first frame %v %v", fs[0].typ, first)
	}
	r.Svc.Wait()
	if rows := r.Usage.All(); len(rows) != 1 || rows[0].Usage.Output != 3 {
		t.Errorf("rows %+v", rows)
	}
}

func TestStreamWithoutUsageSettlesToTheReservation(t *testing.T) {
	for name, c := range map[string]struct{ path, body string }{
		"converse": {modelPath("converse-stream"), converseBody},
		"invoke":   {modelPath("invoke-with-response-stream"), invokeBody},
	} {
		t.Run(name, func(t *testing.T) {
			r := gwtest.New(t)
			r.Fake.OmitStreamUsage(true)
			resp := post(t, r, c.path, c.body, nil)
			_ = readFrames(t, resp.Body)
			r.Svc.Wait()
			if len(r.Spend.Settlements) != 1 || len(r.Spend.Reservs) != 1 || r.Spend.Settlements[0].Actual.String() != r.Spend.Reservs[0].String() {
				t.Errorf("settlements %+v reservations %v", r.Spend.Settlements, r.Spend.Reservs)
			}
		})
	}
}

func TestStreamFailureAfterTheFirstEvent(t *testing.T) {
	t.Run("converse exception frame", func(t *testing.T) {
		r := gwtest.New(t)
		r.Fake.FailStreamAfter(3)
		resp := post(t, r, modelPath("converse-stream"), converseBody, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		fs := readFrames(t, resp.Body)
		last := fs[len(fs)-1]
		var b map[string]any
		_ = json.Unmarshal(last.payload, &b)
		if last.kind != "exception" || b["code"] != "UpstreamError" {
			t.Fatalf("last frame %+v %v", last, b)
		}
		r.Svc.Wait()
		rows := r.Usage.All()
		if len(rows) != 1 || rows[0].Outcome != gateway.OutcomeError || len(r.Spend.Settlements) != 1 {
			t.Errorf("rows %+v settlements %+v", rows, r.Spend.Settlements)
		}
	})
	t.Run("anthropic error event", func(t *testing.T) {
		r := gwtest.New(t)
		r.Fake.FailStreamAfter(3)
		resp := post(t, r, "/anthropic/v1/messages", strings.Replace(anthropicReq, "%s", "true", 1), nil)
		ev, data := sse(t, resp.Body)
		last := len(ev) - 1
		if ev[last] != "error" || data[last]["code"] != "UpstreamError" || data[last]["type"] != "error" {
			t.Fatalf("events %v last %v", ev, data[last])
		}
	})
}

func TestStreamFailureBeforeTheFirstEventIsJSON(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.FailStreamAfter(0)
	resp := post(t, r, modelPath("converse-stream"), converseBody, nil)
	if resp.StatusCode != 502 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if errBody(t, resp)["code"] != "UpstreamError" {
		t.Error("code")
	}
	r.Svc.Wait()
	if r.Spend.Held() != 0 {
		t.Error("the reservation was not released")
	}
}

func TestClientAbortSettlesOnWhatWasSeen(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 20, Output: 9})
	gate := make(chan struct{})
	r.Fake.PauseStreamsOn(gate)
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.HTTP.URL+modelPath("converse-stream"), strings.NewReader(converseBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eventstream.NewDecoder().Decode(resp.Body, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for r.Fake.StreamsEnded() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.Fake.StreamsEnded() == 0 {
		t.Fatal("the upstream stream was not closed after the agent left")
	}
	close(gate)
	for len(r.Spend.Settlements) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		r.Svc.Wait()
	}
	if len(r.Spend.Settlements) != 1 || r.Spend.Held() != 0 {
		t.Errorf("settlements %+v held %d", r.Spend.Settlements, r.Spend.Held())
	}
}

func TestRequestIDIsEchoed(t *testing.T) {
	r := gwtest.New(t)
	resp := post(t, r, modelPath("converse"), `[`, map[string]string{"X-Request-Id": "req-abc.1"})
	if errBody(t, resp)["requestId"] != "req-abc.1" {
		t.Error("request id not echoed")
	}
	resp = post(t, r, modelPath("converse"), `[`, map[string]string{"X-Request-Id": "bad id with spaces"})
	if id, _ := errBody(t, resp)["requestId"].(string); !strings.HasPrefix(id, "gw-") {
		t.Errorf("id %q", id)
	}
}

