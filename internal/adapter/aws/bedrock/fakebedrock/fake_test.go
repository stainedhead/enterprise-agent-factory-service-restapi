package fakebedrock_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock/fakebedrock"
)

func newClient(t *testing.T) (*fakebedrock.Fake, *bedrockruntime.Client) {
	t.Helper()
	f := fakebedrock.New()
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	c := bedrockruntime.New(bedrockruntime.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), HTTPClient: srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Retryer:     aws.NopRetryer{},
	})
	return f, c
}

const profile = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/abc123"

func converseIn() *bedrockruntime.ConverseInput {
	return &bedrockruntime.ConverseInput{
		ModelId: aws.String(profile),
		Messages: []types.Message{{Role: types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "hi"}}}},
	}
}

func TestConverseReportsUsageAndRecordsTheCall(t *testing.T) {
	f, c := newClient(t)
	f.SetUsage(fakebedrock.Usage{Input: 7, Output: 9, CacheRead: 2, CacheWrite: 1})
	out, err := c.Converse(context.Background(), converseIn())
	if err != nil {
		t.Fatal(err)
	}
	if *out.Usage.InputTokens != 7 || *out.Usage.OutputTokens != 9 || *out.Usage.CacheReadInputTokens != 2 || *out.Usage.CacheWriteInputTokens != 1 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	calls := f.Calls()
	if len(calls) != 1 || calls[0].Op != fakebedrock.OpConverse || calls[0].ModelID != profile || calls[0].AccessKeyID != "AKIDEXAMPLE" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestConverseStreamEndsWithMetadata(t *testing.T) {
	f, c := newClient(t)
	f.SetUsage(fakebedrock.Usage{Input: 5, Output: 11})
	out, err := c.ConverseStream(context.Background(), &bedrockruntime.ConverseStreamInput{ModelId: aws.String(profile), Messages: converseIn().Messages})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var meta *types.ConverseStreamMetadataEvent
	for ev := range out.GetStream().Events() {
		switch v := ev.(type) {
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			text.WriteString(v.Value.Delta.(*types.ContentBlockDeltaMemberText).Value)
		case *types.ConverseStreamOutputMemberMetadata:
			meta = &v.Value
		}
	}
	// Once the metadata event (the last frame) has arrived the SDK may close the connection before its decoder
	// goroutine reads the end of the body; on a loaded runner that surfaces as net.ErrClosed. It is not a stream failure.
	if err := out.GetStream().Err(); err != nil && (meta == nil || !errors.Is(err, net.ErrClosed)) {
		t.Fatal(err)
	}
	if text.String() != "Hello from the fake Bedrock runtime" || meta == nil || *meta.Usage.OutputTokens != 11 {
		t.Fatalf("text %q meta %+v", text.String(), meta)
	}
}

func TestStreamFailsPartWay(t *testing.T) {
	f, c := newClient(t)
	f.FailStreamAfter(2)
	out, err := c.ConverseStream(context.Background(), &bedrockruntime.ConverseStreamInput{ModelId: aws.String(profile), Messages: converseIn().Messages})
	if err != nil {
		t.Fatal(err)
	}
	for range out.GetStream().Events() { //nolint:revive // drain
	}
	var ise *types.InternalServerException
	if err := out.GetStream().Err(); !errors.As(err, &ise) {
		t.Fatalf("stream error = %v, want InternalServerException", err)
	}
}

func TestInvokeModelAndStream(t *testing.T) {
	f, c := newClient(t)
	f.SetUsage(fakebedrock.Usage{Input: 3, Output: 4})
	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := c.InvokeModel(context.Background(), &bedrockruntime.InvokeModelInput{ModelId: aws.String(profile), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out.Body), `"output_tokens":4`) {
		t.Fatalf("body %s", out.Body)
	}
	st, err := c.InvokeModelWithResponseStream(context.Background(), &bedrockruntime.InvokeModelWithResponseStreamInput{ModelId: aws.String(profile), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for ev := range st.GetStream().Events() {
		if _, ok := ev.(*types.ResponseStreamMemberChunk); ok {
			n++
		}
	}
	if st.GetStream().Err() != nil || n < 5 {
		t.Fatalf("chunks %d err %v", n, st.GetStream().Err())
	}
}

func TestThrottleAndServerErrorModes(t *testing.T) {
	f, c := newClient(t)
	f.SetMode(fakebedrock.ModeThrottle)
	var te *types.ThrottlingException
	if _, err := c.Converse(context.Background(), converseIn()); !errors.As(err, &te) {
		t.Fatalf("err = %v, want ThrottlingException", err)
	}
	f.SetMode(fakebedrock.ModeServerError)
	var ie *types.InternalServerException
	if _, err := c.Converse(context.Background(), converseIn()); !errors.As(err, &ie) {
		t.Fatalf("err = %v, want InternalServerException", err)
	}
}

func TestUnsupportedAndUnsignedRequests(t *testing.T) {
	f := fakebedrock.New()
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()
	for name, tc := range map[string]struct {
		path, auth string
		want       int
	}{
		"guardrail":  {"/guardrail/x/version/1/apply", "AWS4-HMAC-SHA256 Credential=A/x", http.StatusNotFound},
		"unsigned":   {"/model/m/converse", "", http.StatusForbidden},
		"unknown op": {"/model/m/count-tokens", "AWS4-HMAC-SHA256 Credential=A/x", http.StatusNotFound},
	} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+tc.path, strings.NewReader("{}"))
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
}
