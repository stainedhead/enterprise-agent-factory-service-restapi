package mcpwire

import (
	"encoding/json"
	"net/http"
	"strings"
)

// HTTP header names.
const (
	HeaderProtocolVersion = "MCP-Protocol-Version"
	HeaderSessionID       = "Mcp-Session-Id"
	HeaderMethod          = "Mcp-Method"
	HeaderName            = "Mcp-Name"
)

// NameOf returns the primitive a request names, for the Mcp-Name header and admission: the tool or
// prompt name for tools/call and prompts/get, the URI for resources/read, resources/subscribe and
// resources/unsubscribe, and "" for every other method.
func NameOf(method string, params json.RawMessage) string {
	var key string
	switch method {
	case "tools/call", "prompts/get":
		key = "name"
	case "resources/read", "resources/subscribe", "resources/unsubscribe":
		key = "uri"
	default:
		return ""
	}
	var p map[string]json.RawMessage
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(p[key], &s) != nil {
		return ""
	}
	return s
}

// Version returns the protocol version of a request: the header, or 2025-11-25 when absent.
func Version(h http.Header) string {
	if v := strings.TrimSpace(h.Get(HeaderProtocolVersion)); v != "" {
		return v
	}
	return Version20251125
}

// CheckHeaders verifies Mcp-Method and Mcp-Name against the body (gateway contract §13.2). They are
// required on 2026-07-28 requests and checked whenever present. Mcp-Name is required only for methods
// that name a primitive. A mismatch returns a HeaderMismatch error ready to send.
func CheckHeaders(h http.Header, m *Message) *Error {
	version := Version(h)
	method, hasMethod := h.Get(HeaderMethod), len(h.Values(HeaderMethod)) > 0
	name, hasName := h.Get(HeaderName), len(h.Values(HeaderName)) > 0
	want := NameOf(m.Method, m.Params)
	if version == Version20260728 && m.Method != "" {
		if !hasMethod {
			return mismatch("Mcp-Method header is required")
		}
		if want != "" && !hasName {
			return mismatch("Mcp-Name header is required")
		}
	}
	if hasMethod && method != m.Method {
		return mismatch("Mcp-Method does not match the body")
	}
	if hasName && name != want {
		return mismatch("Mcp-Name does not match the body")
	}
	return nil
}

func mismatch(msg string) *Error {
	d, _ := json.Marshal(map[string]string{"reason": "header_mismatch"})
	return &Error{Code: CodeHeaderMismatch, Message: msg, Data: d}
}
