package gatewayhttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// errClientWrite marks a failed write to the agent: the agent left, which is an abort (GW-6), not a fault.
var errClientWrite = fmt.Errorf("client write failed: %w", context.Canceled)

// exceptionOf reads a Bedrock exception frame as the gateway error it stands for.
func exceptionOf(m eventstream.Message) error {
	typ := m.Headers.Get(":exception-type").String()
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(m.Payload, &body)
	msg := body.Message
	if msg == "" {
		msg = "the model reported an error"
	}
	return &domain.Error{Kind: kindOfException(typ), Message: msg}
}

// stream forwards a streamed call. It reads the first upstream event before it sends anything, so a
// failure to start is still an ordinary JSON error (gateway contract §7); after that the stream carries
// its own end: an exception frame (Bedrock) or an error event (Anthropic) with the same code.
// It returns what the stream reported, so the call settles on the tokens seen even when it is cut.
func (s *Server) stream(ctx context.Context, rw *respWriter, p *parsed, call bedrock.Call) (gateway.Result, error) {
	var meter gateway.StreamMeter
	st, err := s.up.Open(ctx, call)
	if err != nil {
		return gateway.Result{}, err
	}
	defer st.Body.Close()
	dec := eventstream.NewDecoder()
	first, err := dec.Decode(st.Body, nil)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = &domain.Error{Kind: domain.KindUpstream, Message: "the model returned an empty stream"}
		} else if !errors.Is(err, context.Canceled) {
			err = &domain.Error{Kind: domain.KindUpstream, Message: "the model stream could not be read", Cause: err}
		}
		return gateway.Result{}, err
	}
	if first.Headers.Get(":message-type").String() == "exception" {
		return gateway.Result{}, exceptionOf(first)
	}
	meter.Started()

	var out frameWriter
	if p.wire == wireAnthropic {
		rw.Header().Set("Content-Type", "text/event-stream")
		out = &sseWriter{rw: rw}
	} else {
		rw.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		out = &eventWriter{rw: rw, enc: eventstream.NewEncoder()}
	}
	rw.WriteHeader(http.StatusOK)

	msg := first
	for {
		if msg.Headers.Get(":message-type").String() == "exception" {
			ex := exceptionOf(msg)
			_ = out.fail(ex, rw.requestID)
			return meter.Result(), ex
		}
		observe(&meter, p.wire, msg)
		if err := out.event(msg); err != nil {
			return meter.Result(), err
		}
		next, err := dec.Decode(st.Body, nil)
		if errors.Is(err, io.EOF) {
			return meter.Result(), nil
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return meter.Result(), context.Canceled
			}
			ex := &domain.Error{Kind: domain.KindUpstream, Message: "the model stream was cut", Cause: err}
			_ = out.fail(ex, rw.requestID)
			return meter.Result(), ex
		}
		msg = next
	}
}

// frameWriter writes the events of one wire format.
type frameWriter interface {
	event(m eventstream.Message) error
	fail(err error, requestID string) error
}

// eventWriter re-encodes Bedrock event-stream frames.
type eventWriter struct {
	rw  *respWriter
	enc *eventstream.Encoder
}

func (w *eventWriter) event(m eventstream.Message) error {
	var buf bytes.Buffer
	if err := w.enc.Encode(&buf, m); err != nil {
		return err
	}
	if _, err := w.rw.Write(buf.Bytes()); err != nil {
		return errClientWrite
	}
	w.rw.flush()
	return nil
}

// fail ends the stream with an exception frame carrying the contract code (§5).
func (w *eventWriter) fail(err error, requestID string) error {
	_, p := toProblem(err, requestID, false)
	b, _ := json.Marshal(p)
	var hs eventstream.Headers
	hs.Set(":message-type", eventstream.StringValue("exception"))
	hs.Set(":exception-type", eventstream.StringValue(exceptionType(domain.KindOf(err))))
	hs.Set(":content-type", eventstream.StringValue("application/json"))
	return w.event(eventstream.Message{Headers: hs, Payload: b})
}

// sseWriter turns Bedrock's {"bytes": base64} chunks into the Anthropic server-sent events they carry.
type sseWriter struct{ rw *respWriter }

func (w *sseWriter) event(m eventstream.Message) error {
	var c struct {
		Bytes []byte `json:"bytes"`
	}
	if json.Unmarshal(m.Payload, &c) != nil || len(c.Bytes) == 0 {
		return nil // not a chunk; nothing an Anthropic client reads
	}
	var t struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(c.Bytes, &t)
	return w.send(t.Type, c.Bytes)
}

func (w *sseWriter) send(event string, data []byte) error {
	var buf bytes.Buffer
	buf.WriteString("event: " + event + "\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	if _, err := w.rw.Write(buf.Bytes()); err != nil {
		return errClientWrite
	}
	w.rw.flush()
	return nil
}

// fail ends the stream with an Anthropic error event carrying the contract code.
func (w *sseWriter) fail(err error, requestID string) error {
	_, p := toProblem(err, requestID, true)
	b, _ := json.Marshal(p)
	return w.send("error", b)
}

// observe feeds the meter from an event: Converse metadata, Anthropic message_start / message_delta and
// the invocation metrics of message_stop. The base64 decoding only runs for Invoke-style chunks.
func observe(m *gateway.StreamMeter, wire wireKind, msg eventstream.Message) {
	switch wire {
	case wireConverse:
		if msg.Headers.Get(":event-type").String() != "metadata" {
			return
		}
		var d struct {
			Usage usageJSON `json:"usage"`
		}
		if json.Unmarshal(msg.Payload, &d) == nil {
			if u, ok := d.Usage.usage(); ok {
				m.Observe(u)
			}
		}
	default:
		var c struct {
			Bytes string `json:"bytes"`
		}
		if json.Unmarshal(msg.Payload, &c) != nil || c.Bytes == "" {
			return
		}
		raw, err := base64.StdEncoding.DecodeString(c.Bytes)
		if err != nil {
			return
		}
		var d struct {
			Type    string    `json:"type"`
			Usage   usageJSON `json:"usage"`
			Message struct {
				Usage usageJSON `json:"usage"`
			} `json:"message"`
			Metrics *struct {
				In  *int `json:"inputTokenCount"`
				Out *int `json:"outputTokenCount"`
			} `json:"amazon-bedrock-invocationMetrics"`
		}
		if json.Unmarshal(raw, &d) != nil {
			return
		}
		if u, ok := d.Message.Usage.usage(); ok && d.Type == "message_start" {
			m.Observe(u)
		}
		if u, ok := d.Usage.usage(); ok && d.Type == "message_delta" {
			m.Observe(u)
		}
		if d.Metrics != nil {
			m.Observe(gateway.Usage{Input: val(d.Metrics.In), Output: val(d.Metrics.Out), CacheRead: gateway.Absent, CacheWrite: gateway.Absent})
		}
	}
}
