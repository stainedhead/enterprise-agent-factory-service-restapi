package edgehttp

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455 handshake, not a security hash
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

// maxTextMessage is the largest WebSocket text message the edge buffers to scan; a larger one closes the
// connection with 1009.
const maxTextMessage = 1 << 20

// wsGUID is the RFC 6455 handshake constant.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket close codes the edge sends.
const (
	closeProtocol   = 1002
	closePolicy     = 1008
	closeTooBig     = 1009
	opContinuation  = 0x0
	opText          = 0x1
	opBinary        = 0x2
	opClose         = 0x8
	opPong          = 0xA
	maxControlBytes = 125
)

var errClosed = errors.New("edgehttp: websocket closed by policy")

// framePolicy decides what a client may send over an upgraded connection (PC-D17).
type framePolicy struct {
	// CanSend is false for a caller holding only collaboration:access: any data frame closes with 1008.
	CanSend bool
	// Scan reports whether a complete text message must be refused (the secret scan).
	Scan func(text []byte) (block bool)
}

func closeFrame(code int) []byte {
	return []byte{0x88, 0x02, byte(code >> 8), byte(code)} //nolint:gosec // close codes fit in 16 bits
}

type frame struct {
	fin    bool
	opcode byte
	mask   [4]byte
	masked bool
	// rsv is true when a reserved bit is set: no extension is ever negotiated, so any is a violation.
	rsv bool
	n   uint64
	raw []byte // the header bytes as received
}

func readHeader(r *bufio.Reader) (frame, error) {
	var f frame
	b := make([]byte, 2, 14)
	if _, err := io.ReadFull(r, b); err != nil {
		return f, err
	}
	f.fin, f.opcode, f.masked, f.rsv = b[0]&0x80 != 0, b[0]&0x0f, b[1]&0x80 != 0, b[0]&0x70 != 0
	n := uint64(b[1] & 0x7f)
	switch n {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return f, err
		}
		b = append(b, ext...)
		n = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return f, err
		}
		b = append(b, ext...)
		n = binary.BigEndian.Uint64(ext)
		if n > 1<<40 {
			return f, errors.New("edgehttp: websocket frame too large")
		}
	}
	if f.masked {
		if _, err := io.ReadFull(r, f.mask[:]); err != nil {
			return f, err
		}
		b = append(b, f.mask[:]...)
	}
	f.n, f.raw = n, b
	return f, nil
}

func unmask(p []byte, key [4]byte) {
	for i := range p {
		p[i] ^= key[i%4]
	}
}

// relayClientFrames copies client-to-server frames from src to dst under the policy. A refusal writes a
// close frame to client and returns errClosed. Server-to-client bytes are not parsed.
func relayClientFrames(src *bufio.Reader, dst, client io.Writer, p framePolicy) error {
	refuse := func(code int) error {
		_, _ = client.Write(closeFrame(code))
		return errClosed
	}
	var (
		kind    byte // opText or opBinary while a fragmented message is open, else 0
		held    [][]byte
		text    []byte
		heldLen int
	)
	for {
		f, err := readHeader(src)
		if err != nil {
			return err
		}
		// No extension is negotiated, so reserved bits (compression) and reserved opcodes (3-7, 0xB-0xF) are
		// protocol errors; letting them through would hide frames from the scan (finding 3).
		if f.rsv || !f.masked || (f.opcode > opBinary && f.opcode < opClose) || f.opcode > opPong {
			return refuse(closeProtocol)
		}
		if f.opcode >= opClose {
			if f.n > maxControlBytes || !f.fin {
				return refuse(closeProtocol)
			}
			buf := make([]byte, f.n)
			if _, err := io.ReadFull(src, buf); err != nil {
				return err
			}
			if _, err := dst.Write(append(append([]byte{}, f.raw...), buf...)); err != nil {
				return err
			}
			if f.opcode == opClose {
				return nil
			}
			continue
		}
		if !p.CanSend {
			return refuse(closePolicy)
		}
		if f.opcode != opContinuation {
			if kind != 0 {
				return refuse(closeProtocol)
			}
			kind = f.opcode
		} else if kind == 0 {
			return refuse(closeProtocol)
		}
		heldLen += int(f.n) //nolint:gosec // n is bounded by readHeader (2^40)
		if heldLen > maxTextMessage || f.n > maxTextMessage {
			return refuse(closeTooBig)
		}
		payload := make([]byte, f.n)
		if _, err := io.ReadFull(src, payload); err != nil {
			return err
		}
		held = append(held, append(append([]byte{}, f.raw...), payload...))
		if f.masked {
			unmask(payload, f.mask)
		}
		text = append(text, payload...)
		if !f.fin {
			continue
		}
		// Text and binary messages are scanned alike (binary only when it is valid UTF-8, so a real file is
		// not matched by accident); a fragmented message is scanned once reassembled.
		if p.Scan != nil && (kind == opText || utf8.Valid(text)) && p.Scan(text) {
			return refuse(closePolicy)
		}
		for _, fr := range held {
			if _, err := dst.Write(fr); err != nil {
				return err
			}
		}
		kind, held, text, heldLen = 0, nil, nil, 0
	}
}

// idleConn refreshes a deadline on every read and write, so a connection with no traffic for d is closed.
type idleConn struct {
	net.Conn
	d time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.d))
	return c.Conn.Read(p)
}

func (c idleConn) Write(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.d))
	return c.Conn.Write(p)
}

// syncWriter serializes writes to the client: the upstream copy and the edge's own close frame share it.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // RFC 6455 handshake, not a security hash
	return base64.StdEncoding.EncodeToString(h[:])
}

// validHandshake checks the upstream's answer: 101, the Upgrade and Connection tokens, the Accept value
// for the client's key, and no extension (none was offered).
func validHandshake(resp *http.Response, key string) bool {
	return resp.StatusCode == http.StatusSwitchingProtocols &&
		strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") && headerHasToken(resp.Header, "Connection", "upgrade") &&
		resp.Header.Get("Sec-WebSocket-Accept") == acceptKey(key) && resp.Header.Get("Sec-WebSocket-Extensions") == ""
}

// hopByHop headers never relayed from an upstream refusal.
var hopByHop = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Upgrade", "Te", "Trailer"}

// relayRefusal passes the upstream's non-101 answer to the client as an ordinary response.
func relayRefusal(w http.ResponseWriter, resp *http.Response, id string) {
	h := w.Header()
	for k, v := range resp.Header {
		h[k] = v
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
	h.Del("Content-Length")
	securityHeaders(h)
	scrubSetCookies(h)
	h.Set("X-Request-Id", id)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// tunnel carries a WebSocket upgrade to the team's Paperclip: the upstream's handshake answer is validated
// before anything is relayed (a non-101 answer goes to the client as a plain response and the connection
// closes), server-to-client bytes are copied, client-to-server frames pass through the policy, and the
// stream is authorized again every RecheckEvery. Both directions end after IdleTimeout without traffic.
func (s *Server) tunnel(w http.ResponseWriter, r *http.Request, res edge.Result, out *http.Request, rq edge.Request) {
	target, err := url.Parse(res.Upstream)
	if err != nil {
		s.fail(w, r, res, 503)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if kb, err := base64.StdEncoding.DecodeString(key); err != nil || len(kb) != 16 || len(r.Header.Values("Sec-WebSocket-Key")) != 1 {
		writeError(w, r, edge.Result{Status: 400, Code: "ValidationError", Message: "The WebSocket handshake is not valid."}, w.Header().Get("X-Request-Id"), false)
		return
	}
	dctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	up, err := s.dial(dctx, "tcp", target.Host)
	cancel()
	if err != nil {
		s.fail(w, r, res, 503)
		return
	}
	upstream := idleConn{Conn: up, d: s.idle}
	defer func() { _ = upstream.Close() }()
	if err := out.Write(upstream); err != nil {
		s.fail(w, r, res, 503)
		return
	}
	upBR := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upBR, out)
	if err != nil {
		s.fail(w, r, res, 503)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		relayRefusal(w, resp, w.Header().Get("X-Request-Id"))
		return
	}
	if !validHandshake(resp, key) {
		s.log.Warn("upstream websocket handshake is not valid", "afs.team_id", res.Route.TeamID)
		s.fail(w, r, res, 503)
		return
	}
	cc, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.fail(w, r, res, 503)
		return
	}
	rawClient := idleConn{Conn: cc, d: s.idle}
	defer func() { _ = rawClient.Close() }()
	client := &syncWriter{w: rawClient}
	scrubSetCookies(resp.Header)
	if err := resp.Write(client); err != nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(client, upBR)
		_ = rawClient.Close()
	}()
	stop := make(chan struct{})
	defer close(stop)
	go s.watch(r.Context(), rq, res, stop, func() {
		_, _ = client.Write(closeFrame(closePolicy))
		_ = rawClient.Close()
		_ = upstream.Close()
	})
	policy := framePolicy{CanSend: s.svc.FrameAllowed(res)}
	if res.ScanMode != "" {
		policy.Scan = func(text []byte) bool {
			matches := collab.ScanMessage(text)
			if len(matches) == 0 {
				return false
			}
			_, blocked := s.svc.OnScan(r.Context(), res, matches)
			return blocked
		}
	}
	_ = relayClientFrames(brw.Reader, upstream, client, policy)
	_ = upstream.Close()
	<-done
}

// isUpgrade reports a WebSocket handshake.
func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && headerHasToken(r.Header, "Connection", "upgrade")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
