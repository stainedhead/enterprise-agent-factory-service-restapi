package middleware

import (
	"bytes"
	"net/http"
)

// bufferedWriter holds a response until the handler returns, so a middleware can still change the
// headers or replace the response. A body larger than limit stops being buffered: what was held is
// written out and the rest streams through (limit 0 means no limit).
type bufferedWriter struct {
	http.ResponseWriter
	limit       int
	status      int
	body        bytes.Buffer
	passthrough bool
	finished    bool
}

// WriteHeader implements http.ResponseWriter.
func (b *bufferedWriter) WriteHeader(code int) {
	if b.passthrough {
		b.ResponseWriter.WriteHeader(code)
		return
	}
	if b.status == 0 {
		b.status = code
	}
}

// Write implements http.ResponseWriter.
func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.passthrough {
		return b.ResponseWriter.Write(p)
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if b.limit > 0 && b.body.Len()+len(p) > b.limit {
		b.passthrough = true
		b.ResponseWriter.WriteHeader(b.status)
		if b.body.Len() > 0 {
			_, _ = b.ResponseWriter.Write(b.body.Bytes())
			b.body.Reset()
		}
		return b.ResponseWriter.Write(p)
	}
	return b.body.Write(p)
}

// Flush implements http.Flusher by releasing the buffer: a handler that flushes is streaming.
func (b *bufferedWriter) Flush() {
	if !b.passthrough {
		b.passthrough = true
		if b.status == 0 {
			b.status = http.StatusOK
		}
		b.ResponseWriter.WriteHeader(b.status)
		_, _ = b.ResponseWriter.Write(b.body.Bytes())
		b.body.Reset()
	}
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap supports http.ResponseController.
func (b *bufferedWriter) Unwrap() http.ResponseWriter { return b.ResponseWriter }

// finish lets adjust change the held response, then writes it. It does nothing when the response
// already streamed out.
func (b *bufferedWriter) finish(adjust func(h http.Header, status int, body []byte)) {
	if b.passthrough || b.finished {
		return
	}
	b.finished = true
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	if adjust != nil {
		adjust(b.Header(), status, b.body.Bytes())
	}
	b.ResponseWriter.WriteHeader(status)
	if b.body.Len() > 0 {
		_, _ = b.ResponseWriter.Write(b.body.Bytes())
	}
}
