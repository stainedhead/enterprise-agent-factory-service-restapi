//go:build conformance

// Package conformance proves gateway contract v1.0.0 from the agent's side: the standard AWS SDK for Go
// Bedrock Runtime client and the Anthropic SDK, configured with nothing but the environment of contract
// §3, call the gateway and get what the contract promises. The gateway is the real one (HTTP adapter, use
// case, signing runtime) over in-memory spend and the fake Bedrock; no AWS account is involved.
package conformance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock/fakebedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp/gwtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// agentKey is what the agent's SDK signs with; the gateway must never use or forward it.
const agentKey = "AKIDPODIDENTITYAGENT"

// podEnv is the environment of contract §3 and nothing else (the CA variables are for TLS, which the
// test gateway does not use).
func podEnv(t *testing.T, r *gwtest.Rig) {
	t.Helper()
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", agentKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", "pod-identity-secret")
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", r.HTTP.URL)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("ANTHROPIC_BASE_URL", r.HTTP.URL+"/anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "afs-gateway")
}

func bedrockClient(t *testing.T, r *gwtest.Rig) *bedrockruntime.Client {
	t.Helper()
	podEnv(t, r)
	cfg, err := config.LoadDefaultConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return bedrockruntime.NewFromConfig(cfg)
}

func anthropicClient(t *testing.T, r *gwtest.Rig) *anthropic.Client {
	t.Helper()
	podEnv(t, r)
	c := anthropic.NewClient()
	return &c // ANTHROPIC_BASE_URL and ANTHROPIC_API_KEY from the environment
}

func converseIn(maxTokens int32) *bedrockruntime.ConverseInput {
	return &bedrockruntime.ConverseInput{
		ModelId:         aws.String(gwtest.Model),
		Messages:        []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "hello there"}}}},
		InferenceConfig: &types.InferenceConfiguration{MaxTokens: aws.Int32(maxTokens)},
		RequestMetadata: map[string]string{"afs-team-id": "team_someone_else"},
	}
}

func anthropicIn(maxTokens int64) anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		Model: anthropic.Model(gwtest.Model), MaxTokens: maxTokens,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hello there"))},
	}
}

func invokeBody(maxTokens string) []byte {
	return []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":` + maxTokens + `,"messages":[{"role":"user","content":"hello there"}]}`)
}

// attributed checks the call was the agent's: one usage row for the agent, signed upstream with the
// team's role credentials and never the agent's, with the gateway's tags and not the agent's.
func attributed(t *testing.T, r *gwtest.Rig, wantIn, wantOut int) {
	t.Helper()
	r.Svc.Wait()
	rows := r.Usage.All()
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.TeamID != gwtest.TeamA || row.AgentID != "agt_01" || row.Outcome != gateway.OutcomeOK || row.Usage.Input != wantIn || row.Usage.Output != wantOut {
		t.Errorf("row %+v", row)
	}
	calls := r.Fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d", len(calls))
	}
	if calls[0].AccessKeyID == agentKey || !strings.HasPrefix(calls[0].AccessKeyID, "ASIA") {
		t.Errorf("upstream was signed with %q, not the team role's credentials", calls[0].AccessKeyID)
	}
	if strings.Contains(string(calls[0].Body), "team_someone_else") {
		t.Error("the agent's own requestMetadata reached Bedrock")
	}
}

// reserved is the reservation of the single call so far, in micro-USD.
func reserved(t *testing.T, r *gwtest.Rig) int64 {
	t.Helper()
	r.Svc.Wait()
	if len(r.Spend.Reservs) != 1 {
		t.Fatalf("reservations = %d", len(r.Spend.Reservs))
	}
	return r.Spend.Reservs[0].Micros()
}

func TestBedrockConverse(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 12, Output: 8})
	out, err := bedrockClient(t, r).Converse(t.Context(), converseIn(100000))
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt32(out.Usage.InputTokens) != 12 || aws.ToInt32(out.Usage.OutputTokens) != 8 {
		t.Errorf("usage %+v", out.Usage)
	}
	attributed(t, r, 12, 8)
	// maxTokens 100000 is capped to the model's 4096 output tokens at $15/M, plus input at $3/M.
	if got := reserved(t, r); got < 4096*15+3 || got > 4096*15+3*2000 {
		t.Errorf("reservation = %d micro-USD, want 4096 output tokens plus a few input tokens", got)
	}
	var up struct{ InferenceConfig struct{ MaxTokens int } }
	if err := jsonUnmarshal(r.Fake.Calls()[0].Body, &up); err != nil || up.InferenceConfig.MaxTokens != 4096 {
		t.Errorf("upstream maxTokens = %d (%v), want the model's 4096", up.InferenceConfig.MaxTokens, err)
	}
}

func TestBedrockReservationUsesTheRequestsOwnLimit(t *testing.T) {
	r := gwtest.New(t)
	if _, err := bedrockClient(t, r).Converse(t.Context(), converseIn(200)); err != nil {
		t.Fatal(err)
	}
	if got := reserved(t, r); got < 200*15+3 || got > 200*15+3*2000 {
		t.Errorf("reservation = %d micro-USD, want 200 output tokens plus a few input tokens", got)
	}
}

func TestBedrockConverseStream(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 30, Output: 21})
	r.Fake.SetText("one two three four five")
	out, err := bedrockClient(t, r).ConverseStream(t.Context(), &bedrockruntime.ConverseStreamInput{
		ModelId: aws.String(gwtest.Model), Messages: converseIn(1000).Messages, InferenceConfig: converseIn(1000).InferenceConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := out.GetStream()
	defer stream.Close()
	var text strings.Builder
	var usage *types.TokenUsage
	for ev := range stream.Events() {
		switch v := ev.(type) {
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			if d, ok := v.Value.Delta.(*types.ContentBlockDeltaMemberText); ok {
				text.WriteString(d.Value)
			}
		case *types.ConverseStreamOutputMemberMetadata:
			usage = v.Value.Usage
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text.String()) != "one two three four five" || usage == nil || aws.ToInt32(usage.OutputTokens) != 21 {
		t.Errorf("text %q usage %+v", text.String(), usage)
	}
	attributed(t, r, 30, 21)
}

func TestBedrockInvokeModel(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 9, Output: 4})
	out, err := bedrockClient(t, r).InvokeModel(t.Context(), &bedrockruntime.InvokeModelInput{
		ModelId: aws.String(gwtest.Model), ContentType: aws.String("application/json"), Body: invokeBody("100000")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out.Body), `"usage"`) {
		t.Errorf("body %s", out.Body)
	}
	attributed(t, r, 9, 4)
	if got := reserved(t, r); got < 4096*15 || got > 4096*15+3*2000 {
		t.Errorf("reservation = %d", got)
	}
}

func TestBedrockInvokeModelWithResponseStream(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 6, Output: 5})
	out, err := bedrockClient(t, r).InvokeModelWithResponseStream(t.Context(), &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId: aws.String(gwtest.Model), ContentType: aws.String("application/json"), Body: invokeBody("500")})
	if err != nil {
		t.Fatal(err)
	}
	stream := out.GetStream()
	defer stream.Close()
	chunks := 0
	for ev := range stream.Events() {
		if c, ok := ev.(*types.ResponseStreamMemberChunk); ok && len(c.Value.Bytes) > 0 {
			chunks++
		}
	}
	if err := stream.Err(); err != nil || chunks < 4 {
		t.Fatalf("chunks %d err %v", chunks, err)
	}
	attributed(t, r, 6, 5)
}

func TestAnthropicMessages(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 14, Output: 6})
	msg, err := anthropicClient(t, r).Messages.New(t.Context(), anthropicIn(8000))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Usage.InputTokens != 14 || msg.Usage.OutputTokens != 6 {
		t.Errorf("usage %+v", msg.Usage)
	}
	attributed(t, r, 14, 6)
	if got := reserved(t, r); got < 4096*15 || got > 4096*15+3*2000 {
		t.Errorf("reservation = %d, want max_tokens 8000 capped to 4096", got)
	}
}

func TestAnthropicMessagesStream(t *testing.T) {
	r := gwtest.New(t)
	r.Fake.SetUsage(fakebedrock.Usage{Input: 14, Output: 6})
	r.Fake.SetText("alpha beta gamma")
	stream := anthropicClient(t, r).Messages.NewStreaming(t.Context(), anthropicIn(1000))
	defer stream.Close()
	var text strings.Builder
	var final anthropic.Message
	for stream.Next() {
		ev := stream.Current()
		if err := final.Accumulate(ev); err != nil {
			t.Fatal(err)
		}
		if d, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			text.WriteString(d.Delta.Text)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text.String()) != "alpha beta gamma" || final.Usage.OutputTokens != 6 {
		t.Errorf("text %q usage %+v", text.String(), final.Usage)
	}
	attributed(t, r, 14, 6)
}

// ---- §7 errors, as the SDKs see them ----

type wantErr struct {
	status int
	code   string // gateway contract code
	aws    string // the AWS SDK error code
}

func awsStatus(err error) (int, string) {
	var re *awshttp.ResponseError
	var api smithy.APIError
	code := ""
	if errors.As(err, &api) {
		code = api.ErrorCode()
	}
	if errors.As(err, &re) {
		return re.HTTPStatusCode(), code
	}
	return 0, code
}

func TestErrorsThroughTheSDKs(t *testing.T) {
	cases := map[string]struct {
		setup func(*gwtest.Rig)
		want  wantErr
	}{
		"unknown pod":      {func(r *gwtest.Rig) { r.Dir.Delete("127.0.0.1") }, wantErr{401, "Unauthenticated", "UnrecognizedClientException"}},
		"budget exhausted": {func(r *gwtest.Rig) { r.Spend.Limit = domain.MustMoney("0.000001") }, wantErr{402, "BudgetExceeded", "AccessDeniedException"}},
		"model disabled":   {func(r *gwtest.Rig) { delete(r.Store.Models, gwtest.TeamA+"\x00"+gwtest.Model) }, wantErr{403, "AccessDenied", "AccessDeniedException"}},
		"team suspended": {func(r *gwtest.Rig) {
			tm := r.Store.Teams[gwtest.TeamA]
			tm.State = "suspended"
			r.Store.Teams[gwtest.TeamA] = tm
		}, wantErr{403, "AccessDenied", "AccessDeniedException"}},
		"throttled":      {func(r *gwtest.Rig) { r.Fake.SetMode(fakebedrock.ModeThrottle) }, wantErr{429, "Throttled", "ThrottlingException"}},
		"upstream error": {func(r *gwtest.Rig) { r.Fake.SetMode(fakebedrock.ModeServerError) }, wantErr{502, "UpstreamError", "InternalServerException"}},
	}
	for name, c := range cases {
		t.Run(name+"/bedrock", func(t *testing.T) {
			r := gwtest.New(t)
			c.setup(r)
			_, err := bedrockClient(t, r).Converse(t.Context(), converseIn(100))
			status, code := awsStatus(err)
			if err == nil || status != c.want.status || code != c.want.aws {
				t.Fatalf("status %d code %q err %v, want %d %s", status, code, err, c.want.status, c.want.aws)
			}
		})
		t.Run(name+"/anthropic", func(t *testing.T) {
			r := gwtest.New(t)
			c.setup(r)
			_, err := anthropicClient(t, r).Messages.New(t.Context(), anthropicIn(100), anthropicNoRetry())
			var ae *anthropic.Error
			if !errors.As(err, &ae) || ae.StatusCode != c.want.status {
				t.Fatalf("err %v, want status %d", err, c.want.status)
			}
			if !strings.Contains(ae.RawJSON(), `"code":"`+c.want.code+`"`) {
				t.Errorf("body %s lacks code %s", ae.RawJSON(), c.want.code)
			}
		})
	}
}

func TestBudgetExceededCarriesTheContractDetails(t *testing.T) {
	r := gwtest.New(t, func(r *gwtest.Rig) { r.Spend.Limit = domain.MustMoney("0.000001") })
	_, err := anthropicClient(t, r).Messages.New(t.Context(), anthropicIn(100), anthropicNoRetry())
	var ae *anthropic.Error
	if !errors.As(err, &ae) {
		t.Fatal(err)
	}
	for _, want := range []string{gwtest.BudgetA, `"period"`, `"remainingUsd"`} {
		if !strings.Contains(ae.RawJSON(), want) {
			t.Errorf("body %s lacks %s", ae.RawJSON(), want)
		}
	}
	if r.Svc.Wait(); len(r.Fake.Calls()) != 0 {
		t.Error("a refused call reached Bedrock")
	}
}

func TestValidationAndUnsupported(t *testing.T) {
	r := gwtest.New(t)
	podEnv(t, r)
	for name, c := range map[string]struct {
		path, body string
		status     int
	}{
		"malformed body":      {"/model/" + gwtest.Model + "/converse", `[`, 400},
		"unsupported op":      {"/model/" + gwtest.Model + "/count-tokens", `{}`, 404},
		"unsupported path":    {"/v1/chat/completions", `{}`, 404},
		"body over 20 MB":     {"/model/" + gwtest.Model + "/converse", `{"x":"` + strings.Repeat("a", 21<<20) + `"}`, 413},
		"anthropic no tokens": {"/anthropic/v1/messages", `{"model":"` + gwtest.Model + `"}`, 400},
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.HTTP.URL+c.path, strings.NewReader(c.body))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != c.status || resp.Header.Get("X-Afs-Gateway-Contract-Version") != "1.0.0" {
				t.Errorf("status %d, version %q", resp.StatusCode, resp.Header.Get("X-Afs-Gateway-Contract-Version"))
			}
		})
	}
}

// ---- streams that fail ----

func TestStreamErrors(t *testing.T) {
	t.Run("before the first event the SDK gets an ordinary error", func(t *testing.T) {
		r := gwtest.New(t)
		r.Fake.FailStreamAfter(0)
		_, err := bedrockClient(t, r).ConverseStream(t.Context(), &bedrockruntime.ConverseStreamInput{
			ModelId: aws.String(gwtest.Model), Messages: converseIn(100).Messages})
		if status, _ := awsStatus(err); status != 502 {
			t.Fatalf("err %v (status %d), want 502", err, status)
		}
		r.Svc.Wait()
		if r.Spend.Held() != 0 {
			t.Error("reservation not released")
		}
	})
	t.Run("after the first event Bedrock clients see an exception event", func(t *testing.T) {
		r := gwtest.New(t)
		r.Fake.FailStreamAfter(3)
		out, err := bedrockClient(t, r).ConverseStream(t.Context(), &bedrockruntime.ConverseStreamInput{
			ModelId: aws.String(gwtest.Model), Messages: converseIn(100).Messages})
		if err != nil {
			t.Fatal(err)
		}
		stream := out.GetStream()
		defer stream.Close()
		for range stream.Events() {
		}
		if stream.Err() == nil {
			t.Fatal("the stream ended cleanly after an upstream failure")
		}
		r.Svc.Wait()
		if rows := r.Usage.All(); len(rows) != 1 || rows[0].Outcome != gateway.OutcomeError {
			t.Errorf("rows %+v", rows)
		}
	})
	t.Run("after the first event Anthropic clients see an error event", func(t *testing.T) {
		r := gwtest.New(t)
		r.Fake.FailStreamAfter(3)
		stream := anthropicClient(t, r).Messages.NewStreaming(t.Context(), anthropicIn(100))
		defer stream.Close()
		for stream.Next() {
		}
		if stream.Err() == nil {
			t.Fatal("the stream ended cleanly after an upstream failure")
		}
	})
	t.Run("a stream with no usage settles to the reservation", func(t *testing.T) {
		r := gwtest.New(t)
		r.Fake.OmitStreamUsage(true)
		out, err := bedrockClient(t, r).ConverseStream(t.Context(), &bedrockruntime.ConverseStreamInput{
			ModelId: aws.String(gwtest.Model), Messages: converseIn(100).Messages, InferenceConfig: converseIn(100).InferenceConfig})
		if err != nil {
			t.Fatal(err)
		}
		for range out.GetStream().Events() {
		}
		r.Svc.Wait()
		set := r.Spend.Settled()
		if len(set) != 1 || set[0].Actual.Micros() != reserved(t, r) {
			t.Errorf("settlements %+v, want the reservation", set)
		}
	})
	t.Run("an agent that disconnects mid-stream is settled and released", func(t *testing.T) {
		r := gwtest.New(t)
		gate := make(chan struct{})
		r.Fake.PauseStreamsOn(gate)
		ctx, cancel := context.WithCancel(t.Context())
		out, err := bedrockClient(t, r).ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
			ModelId: aws.String(gwtest.Model), Messages: converseIn(100).Messages})
		if err != nil {
			t.Fatal(err)
		}
		<-out.GetStream().Events()
		cancel()
		close(gate)
		_ = out.GetStream().Close()
		waitFor(t, func() bool { r.Svc.Wait(); return len(r.Spend.Settled()) == 1 })
		if r.Spend.Held() != 0 {
			t.Error("the reservation was not released")
		}
	})
}
