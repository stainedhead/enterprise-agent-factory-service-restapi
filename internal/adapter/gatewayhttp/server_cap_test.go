package gatewayhttp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp/gwtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// The rig's model allows 4096 output tokens; prices are 3 in and 15 out per million tokens.
const rigCap = 4096

// forwardedLimit reads the output limit at path from the body Bedrock received.
func forwardedLimit(t *testing.T, body []byte, path ...string) (float64, bool) {
	t.Helper()
	var cur any
	if err := json.Unmarshal(body, &cur); err != nil {
		t.Fatal(err)
	}
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		if cur, ok = m[k]; !ok {
			return 0, false
		}
	}
	n, ok := cur.(float64)
	return n, ok
}

// TestOutputLimitIsAlwaysSentAndEqualsTheReservation covers FR-R04 AC1 to AC4: whatever the request says
// about its limit, Bedrock gets a limit no higher than the reserved cap, and the reservation was priced
// on exactly that limit.
func TestOutputLimitIsAlwaysSentAndEqualsTheReservation(t *testing.T) {
	msgs := `"messages":[{"role":"user","content":[{"text":"hi"}]}]`
	type tc struct {
		path, body string
		limit      []string
		want       float64
		keeps      string // a member that must survive in the forwarded body
	}
	conv := func(extra string) string { return `{` + msgs + extra + `}` }
	inv := func(extra string) string { return `{"anthropic_version":"bedrock-2023-05-31",` + msgs + extra + `}` }
	ant := func(extra string) string {
		return `{"model":"` + gwtest.Model + `","stream":false,` + msgs + extra + `}`
	}
	ic := []string{"inferenceConfig", "maxTokens"}
	mt := []string{"max_tokens"}
	cases := map[string]tc{
		"converse absent":                {modelPath("converse"), conv(``), ic, rigCap, ""},
		"converse absent keeps config":   {modelPath("converse"), conv(`,"inferenceConfig":{"temperature":0.5}`), ic, rigCap, "temperature"},
		"converse below":                 {modelPath("converse"), conv(`,"inferenceConfig":{"maxTokens":100}`), ic, 100, ""},
		"converse above":                 {modelPath("converse"), conv(`,"inferenceConfig":{"maxTokens":100000,"topP":0.9}`), ic, rigCap, "topP"},
		"converse invalid value":         {modelPath("converse"), conv(`,"inferenceConfig":{"maxTokens":"many"}`), ic, rigCap, ""},
		"converse null config":           {modelPath("converse"), conv(`,"inferenceConfig":null`), ic, rigCap, ""},
		"converse-stream absent":         {modelPath("converse-stream"), conv(``), ic, rigCap, ""},
		"invoke absent":                  {modelPath("invoke"), inv(``), mt, rigCap, ""},
		"invoke below":                   {modelPath("invoke"), inv(`,"max_tokens":50`), mt, 50, ""},
		"invoke above":                   {modelPath("invoke"), inv(`,"max_tokens":100000`), mt, rigCap, ""},
		"invoke-stream absent":           {modelPath("invoke-with-response-stream"), inv(``), mt, rigCap, ""},
		"anthropic below":                {"/anthropic/v1/messages", ant(`,"max_tokens":7`), mt, 7, ""},
		"anthropic above":                {"/anthropic/v1/messages", ant(`,"max_tokens":100000`), mt, rigCap, ""},
		"anthropic stream above":         {"/anthropic/v1/messages", strings.Replace(ant(`,"max_tokens":100000`), `"stream":false`, `"stream":true`, 1), mt, rigCap, ""},
		"invoke family titan absent":     {"/model/amazon.titan-text-express-v1/invoke", `{"inputText":"hi"}`, []string{"textGenerationConfig", "maxTokenCount"}, 0, ""},
		"invoke family llama absent":     {"/model/meta.llama3-70b-instruct-v1:0/invoke", `{"prompt":"hi"}`, []string{"max_gen_len"}, 0, ""},
		"invoke family titan above":      {"/model/amazon.titan-text-express-v1/invoke", `{"inputText":"hi","textGenerationConfig":{"maxTokenCount":99999,"temperature":0.1}}`, []string{"textGenerationConfig", "maxTokenCount"}, 0, "temperature"},
		"invoke inference profile alias": {"/model/us.anthropic.claude-3-5-sonnet-20241022-v2:0/invoke", inv(``), mt, 0, ""},
	}
	price := gateway.Price{Input: domain.MustMoney("3"), Output: domain.MustMoney("15")}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := gwtest.New(t, func(r *gwtest.Rig) {
				// The other families run against a model of the same cap and price, enabled for team A.
				for _, id := range []string{"amazon.titan-text-express-v1", "meta.llama3-70b-instruct-v1:0", "us.anthropic.claude-3-5-sonnet-20241022-v2:0"} {
					r.Store.Models[gwtest.TeamA+"\x00"+id] = r.Store.Models[gwtest.TeamA+"\x00"+gwtest.Model]
					r.Store.PriceRows[id] = r.Store.PriceRows[gwtest.Model]
				}
			})
			resp := post(t, r, c.path, c.body, nil) //nolint:bodyclose // closed by post's cleanup
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
			}
			_ = readAll(t, resp)
			r.Svc.Wait()
			calls := r.Fake.Calls()
			if len(calls) != 1 {
				t.Fatalf("upstream calls %d", len(calls))
			}
			got, ok := forwardedLimit(t, calls[0].Body, c.limit...)
			if !ok {
				t.Fatalf("no output limit reached Bedrock: %s", calls[0].Body)
			}
			want := c.want
			if want == 0 {
				want = rigCap
			}
			if got != want {
				t.Fatalf("forwarded limit %v, want %v", got, want)
			}
			if c.keeps != "" && !strings.Contains(string(calls[0].Body), c.keeps) {
				t.Fatalf("member %q lost: %s", c.keeps, calls[0].Body)
			}
			// FR-R04 AC4: the reservation is priced on the forwarded limit and nothing else.
			if len(r.Spend.Reservs) != 1 {
				t.Fatalf("reservations %d", len(r.Spend.Reservs))
			}
			wantAmt := gateway.ReserveAmount(price, int(got), len(c.body))
			if r.Spend.Reservs[0] != wantAmt {
				t.Fatalf("reserved %s, forwarded limit %v prices to %s", r.Spend.Reservs[0], got, wantAmt)
			}
		})
	}
}

// TestInvokeWithAnUnknownLimitFieldIsRefusedNotForwarded covers FR-R04 AC2 (Q3): fail closed.
func TestInvokeWithAnUnknownLimitFieldIsRefusedNotForwarded(t *testing.T) {
	r := gwtest.New(t)
	r.Store.Models[gwtest.TeamA+"\x00stability.sd3-large-v1:0"] = r.Store.Models[gwtest.TeamA+"\x00"+gwtest.Model]
	r.Store.PriceRows["stability.sd3-large-v1:0"] = r.Store.PriceRows[gwtest.Model]
	resp := post(t, r, "/model/stability.sd3-large-v1:0/invoke", `{"prompt":"a cat"}`, nil) //nolint:bodyclose // closed by post's cleanup
	if resp.StatusCode != 400 {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
	}
	body := string(readAll(t, resp))
	if !strings.Contains(body, "ValidationError") || !strings.Contains(body, "output limit") {
		t.Errorf("body %s does not name the validation error and the missing limit", body)
	}
	r.Svc.Wait()
	if n := len(r.Fake.Calls()); n != 0 {
		t.Errorf("forwarded %d calls uncapped", n)
	}
	if len(r.Spend.Reservs) != 0 {
		t.Errorf("reserved for a refused call: %v", r.Spend.Reservs)
	}
}

// TestAnthropicMessagesWithoutMaxTokensIsRefusedNotForwarded: the Messages API requires max_tokens, so
// the gateway's answer for its absence is 400 and nothing is reserved or sent (FR-R04 AC3, absent case).
func TestAnthropicMessagesWithoutMaxTokensIsRefusedNotForwarded(t *testing.T) {
	r := gwtest.New(t)
	body := `{"model":"` + gwtest.Model + `","messages":[{"role":"user","content":"hi"}]}`
	resp := post(t, r, "/anthropic/v1/messages", body, nil) //nolint:bodyclose // closed by post's cleanup
	if resp.StatusCode != 400 {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
	}
	r.Svc.Wait()
	if len(r.Fake.Calls()) != 0 || len(r.Spend.Reservs) != 0 {
		t.Errorf("forwarded %d, reserved %d", len(r.Fake.Calls()), len(r.Spend.Reservs))
	}
}
