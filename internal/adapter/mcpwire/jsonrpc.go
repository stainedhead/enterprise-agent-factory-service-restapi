// Package mcpwire is the MCP wire layer shared by afs-gateway and afs-mcp-bridge: JSON-RPC 2.0 messages,
// the MCP header rules of 2026-07-28 and 2025-11-25, sealed session ids, and a Streamable HTTP client.
// It holds no policy; decisions come from internal/domain/mcp.
package mcpwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Protocol versions the gateway speaks (gateway contract §13.2).
const (
	Version20260728 = "2026-07-28"
	Version20251125 = "2025-11-25"
)

// Supported lists the supported versions, newest first.
var Supported = []string{Version20260728, Version20251125}

// IsSupported reports whether v is a supported protocol version.
func IsSupported(v string) bool { return v == Version20260728 || v == Version20251125 }

// JSON-RPC and MCP error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeServerError    = -32000 // AFS refusals that are not about visibility (rate limit, auth, upstream)
	CodeTimeout        = -32001
	CodeHeaderMismatch = -32020
)

// Message is one JSON-RPC 2.0 message: a request (Method and ID), a notification (Method, no ID) or a
// response (ID with Result or Error). ID is kept raw so string and number ids round-trip unchanged.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message) }

// IsRequest reports whether m is a request (has a method and an id).
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification reports whether m is a notification.
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse reports whether m is a response.
func (m *Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// ErrBatch is returned for a JSON array: batching was removed from MCP in 2025-06-18.
var ErrBatch = errors.New("mcpwire: batches are not supported")

// Parse decodes one message and checks its shape. The returned *Error is ready to send back.
func Parse(b []byte) (*Message, *Error) {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		return nil, &Error{Code: CodeInvalidRequest, Message: ErrBatch.Error()}
	}
	var m Message
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&m); err != nil {
		return nil, &Error{Code: CodeParseError, Message: "parse error"}
	}
	if dec.More() {
		return nil, &Error{Code: CodeParseError, Message: "trailing data"}
	}
	if m.JSONRPC != "2.0" {
		return nil, &Error{Code: CodeInvalidRequest, Message: "jsonrpc must be 2.0"}
	}
	if len(m.ID) > 0 {
		switch m.ID[0] {
		case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		default:
			return nil, &Error{Code: CodeInvalidRequest, Message: "id must be a string or number"}
		}
	}
	switch {
	case m.Method != "" && (m.Result != nil || m.Error != nil):
		return nil, &Error{Code: CodeInvalidRequest, Message: "request carries a result"}
	case m.Method == "" && len(m.ID) == 0:
		return nil, &Error{Code: CodeInvalidRequest, Message: "neither request nor response"}
	case m.Method == "" && (m.Result == nil) == (m.Error == nil):
		return nil, &Error{Code: CodeInvalidRequest, Message: "response needs exactly one of result and error"}
	}
	return &m, nil
}

// NewResult builds a response to id.
func NewResult(id json.RawMessage, result any) (*Message, error) {
	r, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: "2.0", ID: id, Result: r}, nil
}

// NewError builds an error response to id. data may be nil.
func NewError(id json.RawMessage, code int, msg string, data any) *Message {
	e := &Error{Code: code, Message: msg}
	if data != nil {
		if d, err := json.Marshal(data); err == nil {
			e.Data = d
		}
	}
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Message{JSONRPC: "2.0", ID: id, Error: e}
}

// NewRequest builds a request.
func NewRequest(id json.RawMessage, method string, params any) (*Message, error) {
	m := &Message{JSONRPC: "2.0", ID: id, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		m.Params = p
	}
	return m, nil
}
