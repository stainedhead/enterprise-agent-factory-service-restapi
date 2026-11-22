package edgehttp

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

// maxTextMessage is the largest WebSocket text message the edge buffers to scan; a larger one closes the
// connection with 1009.
const maxTextMessage = 1 << 20

// WebSocket close codes the edge sends.
const (
	closeProtocol   = 1002
	closePolicy     = 1008
	closeTooBig     = 1009
	opContinuation  = 0x0
	opText          = 0x1
	opBinary        = 0x2
	opClose         = 0x8
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
	n      uint64
	raw    []byte // the header bytes as received
}

func readHeader(r *bufio.Reader) (frame, error) {
	var f frame
	b := make([]byte, 2, 14)
	if _, err := io.ReadFull(r, b); err != nil {
		return f, err
	}
	f.fin, f.opcode, f.masked = b[0]&0x80 != 0, b[0]&0x0f, b[1]&0x80 != 0
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
		if kind != opText {
			// Binary frames are not scanned: stream them.
			if _, err := dst.Write(f.raw); err != nil {
				return err
			}
			n64 := int64(f.n) //nolint:gosec // n is bounded by readHeader (2^40)
			if _, err := io.CopyN(dst, src, n64); err != nil {
				return err
			}
			if f.fin {
				kind = 0
			}
			continue
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
		if p.Scan != nil && p.Scan(text) {
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

// tunnel carries a WebSocket upgrade to the team's Paperclip: the handshake is forwarded, server-to-client
// bytes are copied, client-to-server frames pass through the policy. Both directions end after IdleTimeout
// without traffic.
func (s *Server) tunnel(w http.ResponseWriter, r *http.Request, res edge.Result, out *http.Request) {
	target, err := url.Parse(res.Upstream)
	if err != nil {
		s.fail(w, r, res, 503)
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
	cc, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.fail(w, r, res, 503)
		return
	}
	client := idleConn{Conn: cc, d: s.idle}
	defer func() { _ = client.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
	}()
	policy := framePolicy{CanSend: s.svc.FrameAllowed(res)}
	if res.ScanMode != "" {
		policy.Scan = func(text []byte) bool {
			matches := collab.Scan(text)
			if len(matches) == 0 {
				return false
			}
			_, blocked := s.svc.OnScan(r.Context(), res, matches)
			return blocked
		}
	}
	// The handshake response must reach the client before frames flow: the copy above does that, and
	// client frames are read only after the client has them.
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
