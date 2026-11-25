package fakebedrock_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
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

// Checked on the wire, like TestStreamFailsPartWay: the SDK's event stream reader closes the connection
// once it has the last frame, and under load its decoder goroutine can report "use of closed network
// connection" before the metadata event is delivered (seen in CI). The behavior under test is the fake's:
// text deltas, then a metadata frame with the usage last.
func TestConverseStreamEndsWithMetadata(t *testing.T) {
	f := fakebedrock.New()
	f.SetUsage(fakebedrock.Usage{Input: 5, Output: 11})
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/model/"+url.PathEscape(profile)+"/converse-stream", strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=00")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	dec := eventstream.NewDecoder()
	var text strings.Builder
	var last string
	var usage struct {
		Usage struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
		} `json:"usage"`
	}
	for {
		msg, err := dec.Decode(resp.Body, nil)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		last = msg.Headers.Get(":event-type").String()
		switch last {
		case "contentBlockDelta":
			var d struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(msg.Payload, &d); err != nil {
				t.Fatal(err)
			}
			text.WriteString(d.Delta.Text)
		case "metadata":
			if err := json.Unmarshal(msg.Payload, &usage); err != nil {
				t.Fatal(err)
			}
		}
	}
	if text.String() != "Hello from the fake Bedrock runtime" || last != "metadata" || usage.Usage.OutputTokens != 11 || usage.Usage.InputTokens != 5 {
		t.Fatalf("text %q last %q usage %+v", text.String(), last, usage)
	}
}

// The failure is checked on the wire, not through the SDK's event stream reader: that reader closes the
// connection itself as soon as it has seen the exception, and under load its decoder can lose the race
// and report "use of closed network connection" instead of the exception (reproduced at -count=300 -cpu 1).
// The behavior under test is the fake's: two events, then an exception frame, then the end of the body.
func TestStreamFailsPartWay(t *testing.T) {
	f := fakebedrock.New()
	f.FailStreamAfter(2)
	srv := httptest.NewServer(f.Handler())
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/model/"+url.PathEscape(profile)+"/converse-stream", strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=00")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	dec := eventstream.NewDecoder()
	var types []string
	for {
		msg, err := dec.Decode(resp.Body, nil)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("decode: %v", err)
			}
			break
		}
		kind := msg.Headers.Get(":message-type").String()
		if kind == "exception" {
			kind += ":" + msg.Headers.Get(":exception-type").String()
		}
		types = append(types, kind)
	}
	want := []string{"event", "event", "exception:internalServerException"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("frames = %v, want %v", types, want)
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
	// After the last chunk the SDK may close the connection before its decoder reads the end of the body (see
	// TestConverseStreamEndsWithMetadata); that is not a stream failure.
	if err := st.GetStream().Err(); (err != nil && !strings.Contains(err.Error(), "use of closed network connection")) || n < 5 {
		t.Fatalf("chunks %d err %v", n, err)
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
