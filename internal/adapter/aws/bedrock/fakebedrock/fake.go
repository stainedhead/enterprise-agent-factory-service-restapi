// Package fakebedrock is an in-process Bedrock Runtime that speaks the real wire formats: JSON for
// Converse and InvokeModel, and the AWS event stream for ConverseStream and
// InvokeModelWithResponseStream. It reports usage, can throttle or fail, and can break a stream part
// way, so the gateway's metering, error mapping and streaming are tested without AWS (M3-D1).
//
// The fake never validates a SigV4 signature; it only checks that one is present and records the access
// key id it names, which proves which credentials the gateway signed with.
package fakebedrock

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// Op is a Bedrock Runtime operation.
type Op string

// The four operations the gateway forwards.
const (
	OpConverse                 Op = "converse"
	OpConverseStream           Op = "converse-stream"
	OpInvoke                   Op = "invoke"
	OpInvokeWithResponseStream Op = "invoke-with-response-stream"
)

// Mode is how the fake answers a call.
type Mode int

// Modes.
const (
	// ModeOK answers normally.
	ModeOK Mode = iota
	// ModeThrottle answers 429 ThrottlingException with a Retry-After.
	ModeThrottle
	// ModeServerError answers 500 InternalServerException.
	ModeServerError
	// ModeUnavailable answers 503 ServiceUnavailableException.
	ModeUnavailable
)

// Usage is what the fake reports for a call.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int
}

// Call records one request the fake received.
type Call struct {
	Op          Op
	ModelID     string
	Body        []byte
	Header      http.Header
	AccessKeyID string
	At          time.Time
}

// Fake is the fake service. Its zero value is not usable; use New.
type Fake struct {
	mu          sync.Mutex
	mode        Mode
	usage       Usage
	text        string
	failAfter   int // events sent before an exception, -1 for never
	omitUsage   bool
	delay       time.Duration
	pause       <-chan struct{}
	record      bool
	calls       []Call
	streamsOpen int
	ended       int
}

// New returns a fake that answers with 12 input and 24 output tokens and a short text.
func New() *Fake {
	return &Fake{usage: Usage{Input: 12, Output: 24}, text: "Hello from the fake Bedrock runtime", failAfter: -1, record: true}
}

// SetMode sets how calls are answered.
func (f *Fake) SetMode(m Mode) { f.set(func() { f.mode = m }) }

// SetUsage sets the usage reported for every call.
func (f *Fake) SetUsage(u Usage) { f.set(func() { f.usage = u }) }

// SetText sets the generated text.
func (f *Fake) SetText(s string) { f.set(func() { f.text = s }) }

// FailStreamAfter ends a stream with an exception event after n events; a negative n never fails.
func (f *Fake) FailStreamAfter(n int) { f.set(func() { f.failAfter = n }) }

// OmitStreamUsage leaves the usage out of streams, as when a client aborts before the final event.
func (f *Fake) OmitStreamUsage(b bool) { f.set(func() { f.omitUsage = b }) }

// SetDelay delays every answer, as Bedrock's own latency would.
func (f *Fake) SetDelay(d time.Duration) { f.set(func() { f.delay = d }) }

// PauseStreamsOn makes every stream wait on ch after its first event; close ch to release them. A nil
// channel removes the pause.
func (f *Fake) PauseStreamsOn(ch <-chan struct{}) { f.set(func() { f.pause = ch }) }

// Record turns the call log on or off (a load test turns it off).
func (f *Fake) Record(b bool) { f.set(func() { f.record = b }) }

func (f *Fake) set(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }

// Calls returns the recorded calls.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Reset clears the recorded calls.
func (f *Fake) Reset() { f.set(func() { f.calls = nil }) }

// StreamsEnded returns how many streams have finished (completed, failed or cut by the client).
func (f *Fake) StreamsEnded() int { f.mu.Lock(); defer f.mu.Unlock(); return f.ended }

// Handler returns the HTTP handler of the fake.
func (f *Fake) Handler() http.Handler { return http.HandlerFunc(f.serve) }

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	op, model, ok := parsePath(r.URL.EscapedPath())
	if !ok || r.Method != http.MethodPost {
		writeJSONError(w, http.StatusNotFound, "UnknownOperationException", "unsupported operation")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "ValidationException", "unreadable body")
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		writeJSONError(w, http.StatusForbidden, "MissingAuthenticationToken", "the request is not signed")
		return
	}
	f.mu.Lock()
	mode, usage, text, failAfter, omit, delay, pause := f.mode, f.usage, f.text, f.failAfter, f.omitUsage, f.delay, f.pause
	if f.record {
		f.calls = append(f.calls, Call{Op: op, ModelID: model, Body: body, Header: r.Header.Clone(), AccessKeyID: accessKey(auth), At: time.Now()})
	}
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	switch mode {
	case ModeThrottle:
		w.Header().Set("Retry-After", "2")
		writeJSONError(w, http.StatusTooManyRequests, "ThrottlingException", "Too many requests, please wait before trying again.")
		return
	case ModeServerError:
		writeJSONError(w, http.StatusInternalServerError, "InternalServerException", "The service encountered an internal error.")
		return
	case ModeUnavailable:
		writeJSONError(w, http.StatusServiceUnavailable, "ServiceUnavailableException", "The service is temporarily unavailable.")
		return
	}
	if !json.Valid(body) {
		writeJSONError(w, http.StatusBadRequest, "ValidationException", "the request body is not JSON")
		return
	}
	switch op {
	case OpConverse:
		writeConverse(w, text, usage)
	case OpInvoke:
		writeInvoke(w, model, text, usage)
	case OpConverseStream, OpInvokeWithResponseStream:
		f.streamStart()
		defer f.streamEnd()
		streamOut(w, r, op, model, text, usage, failAfter, omit, pause)
	}
}

func (f *Fake) streamStart() { f.set(func() { f.streamsOpen++ }) }
func (f *Fake) streamEnd()   { f.set(func() { f.streamsOpen--; f.ended++ }) }

// parsePath splits /model/{id}/{operation}; id may be a URL-escaped inference profile ARN.
func parsePath(escaped string) (Op, string, bool) {
	rest, ok := strings.CutPrefix(escaped, "/model/")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		return "", "", false
	}
	model, err := url.PathUnescape(rest[:i])
	if err != nil {
		return "", "", false
	}
	switch op := Op(rest[i+1:]); op {
	case OpConverse, OpConverseStream, OpInvoke, OpInvokeWithResponseStream:
		return op, model, true
	}
	return "", "", false
}

func accessKey(auth string) string {
	_, after, ok := strings.Cut(auth, "Credential=")
	if !ok {
		return ""
	}
	key, _, _ := strings.Cut(after, "/")
	return key
}

func writeJSONError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Amzn-Errortype", typ)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func writeConverse(w http.ResponseWriter, text string, u Usage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"output":     map[string]any{"message": map[string]any{"role": "assistant", "content": []map[string]any{{"text": text}}}},
		"stopReason": "end_turn",
		"usage": map[string]any{"inputTokens": u.Input, "outputTokens": u.Output, "totalTokens": u.Input + u.Output,
			"cacheReadInputTokens": u.CacheRead, "cacheWriteInputTokens": u.CacheWrite},
		"metrics": map[string]any{"latencyMs": 5},
	})
}

func writeInvoke(w http.ResponseWriter, model, text string, u Usage) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Amzn-Bedrock-Input-Token-Count", fmt.Sprint(u.Input))
	w.Header().Set("X-Amzn-Bedrock-Output-Token-Count", fmt.Sprint(u.Output))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_fake01", "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage": map[string]any{"input_tokens": u.Input, "output_tokens": u.Output,
			"cache_read_input_tokens": u.CacheRead, "cache_creation_input_tokens": u.CacheWrite},
	})
}

// frame is one event stream message to send.
type frame struct {
	typ     string // event type or exception type
	payload []byte
	err     bool
}

func streamOut(w http.ResponseWriter, r *http.Request, op Op, model, text string, u Usage, failAfter int, omit bool, pause <-chan struct{}) {
	var frames []frame
	if op == OpConverseStream {
		frames = converseFrames(text, u, omit)
	} else {
		frames = invokeFrames(model, text, u, omit)
	}
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := eventstream.NewEncoder()
	for i, fr := range frames {
		if failAfter >= 0 && i >= failAfter {
			fr = frame{typ: "internalServerException", payload: []byte(`{"message":"The service encountered an internal error."}`), err: true}
		}
		if err := writeFrame(w, enc, fr, op); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
		if fr.err {
			return
		}
		if i == 0 && pause != nil {
			select {
			case <-pause:
			case <-r.Context().Done():
				return
			}
		}
		if r.Context().Err() != nil {
			return
		}
	}
}

func writeFrame(w io.Writer, enc *eventstream.Encoder, fr frame, _ Op) error {
	var hs eventstream.Headers
	if fr.err {
		hs.Set(":message-type", eventstream.StringValue("exception"))
		hs.Set(":exception-type", eventstream.StringValue(fr.typ))
	} else {
		hs.Set(":message-type", eventstream.StringValue("event"))
		hs.Set(":event-type", eventstream.StringValue(fr.typ))
	}
	hs.Set(":content-type", eventstream.StringValue("application/json"))
	var buf bytes.Buffer
	if err := enc.Encode(&buf, eventstream.Message{Headers: hs, Payload: fr.payload}); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func jsonFrame(typ string, v any) frame {
	b, _ := json.Marshal(v)
	return frame{typ: typ, payload: b}
}

func words(text string) []string {
	ws := strings.Fields(text)
	if len(ws) == 0 {
		return []string{""}
	}
	for i := range ws[:len(ws)-1] {
		ws[i] += " "
	}
	return ws
}

func converseFrames(text string, u Usage, omit bool) []frame {
	fs := []frame{jsonFrame("messageStart", map[string]any{"role": "assistant"})}
	for _, wd := range words(text) {
		fs = append(fs, jsonFrame("contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": wd}}))
	}
	fs = append(fs, jsonFrame("contentBlockStop", map[string]any{"contentBlockIndex": 0}),
		jsonFrame("messageStop", map[string]any{"stopReason": "end_turn"}))
	if !omit {
		fs = append(fs, jsonFrame("metadata", map[string]any{
			"usage": map[string]any{"inputTokens": u.Input, "outputTokens": u.Output, "totalTokens": u.Input + u.Output,
				"cacheReadInputTokens": u.CacheRead, "cacheWriteInputTokens": u.CacheWrite},
			"metrics": map[string]any{"latencyMs": 5}}))
	}
	return fs
}

// chunk wraps an Anthropic event as Bedrock's InvokeModelWithResponseStream does: {"bytes": base64}.
func chunk(ev map[string]any) frame {
	b, _ := json.Marshal(ev)
	return jsonFrame("chunk", map[string]any{"bytes": base64.StdEncoding.EncodeToString(b)})
}

func invokeFrames(model, text string, u Usage, omit bool) []frame {
	startUsage := map[string]any{"input_tokens": u.Input, "output_tokens": 1,
		"cache_read_input_tokens": u.CacheRead, "cache_creation_input_tokens": u.CacheWrite}
	if omit {
		startUsage = map[string]any{}
	}
	fs := []frame{
		chunk(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_fake01", "type": "message", "role": "assistant",
			"model": model, "content": []any{}, "stop_reason": nil, "usage": startUsage}}),
		chunk(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
	}
	for _, wd := range words(text) {
		fs = append(fs, chunk(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": wd}}))
	}
	fs = append(fs, chunk(map[string]any{"type": "content_block_stop", "index": 0}))
	md := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}
	stop := map[string]any{"type": "message_stop"}
	if !omit {
		md["usage"] = map[string]any{"output_tokens": u.Output}
		stop["amazon-bedrock-invocationMetrics"] = map[string]any{"inputTokenCount": u.Input, "outputTokenCount": u.Output,
			"invocationLatency": 5, "firstByteLatency": 2}
	}
	return append(fs, chunk(md), chunk(stop))
}
