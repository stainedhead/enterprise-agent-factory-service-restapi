package gatewayhttp

import (
	"bytes"
	"encoding/json"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

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
