package domain

import (
	"io"
	"time"
)

// MaxRequestIDLength is the longest client-sent X-Request-Id that is echoed (API contract §1).
const MaxRequestIDLength = 64

// RequestID identifies one request in responses, errors, logs and audit records.
type RequestID string

// AcceptRequestID returns the client's id when it is 1 to 64 printable ASCII characters, and
// otherwise a newly generated id. A long or invalid id is replaced, never refused.
func AcceptRequestID(client string, now time.Time, entropy io.Reader) RequestID {
	if requestIDOK(client) {
		return RequestID(client)
	}
	return NewRequestID(now, entropy)
}

// NewRequestID generates a request id (a ULID with prefix req_).
func NewRequestID(now time.Time, entropy io.Reader) RequestID {
	id, err := NewID("req", now, entropy)
	if err != nil {
		id, _ = NewID("req", now, nil)
	}
	return RequestID(id)
}

func requestIDOK(s string) bool {
	if s == "" || len(s) > MaxRequestIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
