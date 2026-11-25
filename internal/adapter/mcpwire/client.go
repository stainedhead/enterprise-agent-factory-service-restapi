package mcpwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Client posts JSON-RPC messages to one Streamable HTTP MCP endpoint.
type Client struct {
	HTTP    *http.Client
	URL     string
	Version string // sent as MCP-Protocol-Version
	// MaxResponse caps the bytes read from one response (0 means 8 MiB).
	MaxResponse int64
	// Header, when set, adds headers (for example upstream credentials) to every request.
	Header func(http.Header)
}

// ErrResponseTooLarge is returned when a response passes MaxResponse.
var ErrResponseTooLarge = errors.New("mcpwire: response too large")

// HTTPError is a non-2xx answer from the endpoint.
type HTTPError struct {
	Status     int
	RetryAfter string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("mcpwire: upstream HTTP %d", e.Status) }

// Call sends m and returns the response with m's id. Messages other than that response that arrive on
// an event stream (notifications, server requests) are passed to onOther, which may be nil. A
// notification gets (nil, nil) once the endpoint accepts it with 202.
func (c *Client) Call(ctx context.Context, m *Message, sessionID string, onOther func(*Message)) (*Message, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Version != "" {
		req.Header.Set(HeaderProtocolVersion, c.Version)
	}
	if m.Method != "" {
		req.Header.Set(HeaderMethod, m.Method)
		if n := NameOf(m.Method, m.Params); n != "" {
			req.Header.Set(HeaderName, n)
		}
	}
	if sessionID != "" {
		req.Header.Set(HeaderSessionID, sessionID)
	}
	if c.Header != nil {
		c.Header(req.Header)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	limit := c.MaxResponse
	if limit <= 0 {
		limit = 8 << 20
	}
	r := &capped{r: resp.Body, n: limit}
	if resp.StatusCode == http.StatusAccepted && m.IsNotification() {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &HTTPError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ct {
	case "application/json":
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		out, perr := Parse(b)
		if perr != nil {
			return nil, perr
		}
		return out, nil
	case "text/event-stream":
		return readStream(r, m.ID, onOther)
	}
	return nil, fmt.Errorf("mcpwire: unexpected content type %q", ct)
}

// readStream reads server-sent events until the response to id arrives.
func readStream(r io.Reader, id json.RawMessage, onOther func(*Message)) (*Message, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var data strings.Builder
	dispatch := func() (*Message, error) {
		defer data.Reset()
		if data.Len() == 0 {
			return nil, nil
		}
		msg, perr := Parse([]byte(data.String()))
		if perr != nil {
			return nil, perr
		}
		if msg.IsResponse() && bytes.Equal(msg.ID, id) {
			return msg, nil
		}
		if onOther != nil {
			onOther(msg)
		}
		return nil, nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if msg, err := dispatch(); msg != nil || err != nil {
				return msg, err
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if msg, err := dispatch(); msg != nil || err != nil {
		return msg, err
	}
	return nil, io.ErrUnexpectedEOF
}

type capped struct {
	r   io.Reader
	n   int64
	eof bool
}

// Read passes reads through until n bytes, then reports EOF if the body ended exactly there and
// ErrResponseTooLarge if it did not.
func (c *capped) Read(p []byte) (int, error) {
	if c.eof {
		return 0, io.EOF
	}
	if c.n <= 0 {
		var one [1]byte
		if m, err := c.r.Read(one[:]); m == 0 && errors.Is(err, io.EOF) {
			c.eof = true
			return 0, io.EOF
		}
		return 0, ErrResponseTooLarge
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	if errors.Is(err, io.EOF) {
		c.eof = true
	}
	return n, err
}
