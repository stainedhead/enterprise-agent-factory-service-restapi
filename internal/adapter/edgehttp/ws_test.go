package edgehttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

var testKey = [4]byte{1, 2, 3, 4}

// clientFrame builds a masked client frame.
func clientFrame(fin bool, op byte, payload []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	out := []byte{b0}
	switch n := len(payload); {
	case n < 126:
		out = append(out, 0x80|byte(n)) //nolint:gosec // n < 126
	case n <= 0xffff:
		out = append(out, 0x80|126, byte(n>>8), byte(n)) //nolint:gosec // n <= 0xffff
	default:
		out = append(out, 0x80|127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}
	out = append(out, testKey[:]...)
	m := append([]byte(nil), payload...)
	unmask(m, testKey)
	return append(out, m...)
}

func relay(t *testing.T, p framePolicy, frames ...[]byte) (forwarded, toClient []byte, err error) {
	t.Helper()
	var dst, cl bytes.Buffer
	err = relayClientFrames(bufio.NewReader(bytes.NewReader(bytes.Join(frames, nil))), &dst, &cl, p)
	return dst.Bytes(), cl.Bytes(), err
}

func TestRelayFrames(t *testing.T) {
	text := clientFrame(true, opText, []byte("hello"))
	bin := clientFrame(true, opBinary, []byte{1, 2, 3})
	ping := clientFrame(true, 0x9, []byte("p"))
	closeF := clientFrame(true, opClose, []byte{0x03, 0xe8})
	long := clientFrame(true, opText, bytes.Repeat([]byte("a"), 300))
	huge := clientFrame(true, opBinary, bytes.Repeat([]byte("b"), 70000))

	// a caller who may contribute passes everything, in order
	fw, cl, err := relay(t, framePolicy{CanSend: true}, text, bin, ping, long, huge, closeF)
	if err != nil || len(cl) != 0 || !bytes.Equal(fw, bytes.Join([][]byte{text, bin, ping, long, huge, closeF}, nil)) {
		t.Fatalf("pass: %v %d", err, len(cl))
	}
	// a watcher may ping and close, but any data frame closes with 1008
	fw, cl, err = relay(t, framePolicy{}, ping, text)
	if !errors.Is(err, errClosed) || !bytes.Equal(fw, ping) || !bytes.Equal(cl, closeFrame(1008)) {
		t.Fatalf("watcher: %v %v %v", err, fw, cl)
	}
	if _, cl, err = relay(t, framePolicy{}, bin); !errors.Is(err, errClosed) || !bytes.Equal(cl, closeFrame(1008)) {
		t.Fatalf("watcher binary: %v", err)
	}
	if fw, _, err = relay(t, framePolicy{}, closeF); err != nil || !bytes.Equal(fw, closeF) {
		t.Fatalf("watcher close: %v", err)
	}
}

func TestRelayScan(t *testing.T) {
	block := framePolicy{CanSend: true, Scan: func(b []byte) bool { return bytes.Contains(b, []byte("SECRET")) }}
	// a clean fragmented message is forwarded whole
	f1, f2 := clientFrame(false, opText, []byte("he")), clientFrame(true, opContinuation, []byte("llo"))
	fw, _, err := relay(t, block, f1, f2)
	if !errors.Is(err, io.EOF) || !bytes.Equal(fw, append(append([]byte{}, f1...), f2...)) {
		t.Fatalf("%v", err)
	}
	// a secret split across fragments is found, and nothing of the message is forwarded
	a, b := clientFrame(false, opText, []byte("xxSEC")), clientFrame(true, opContinuation, []byte("RETxx"))
	fw, cl, err := relay(t, block, a, b)
	if !errors.Is(err, errClosed) || len(fw) != 0 || !bytes.Equal(cl, closeFrame(1008)) {
		t.Fatalf("split secret: %v %d", err, len(fw))
	}
	// binary is not scanned
	if _, _, err = relay(t, block, clientFrame(true, opBinary, []byte("SECRET"))); !errors.Is(err, io.EOF) {
		t.Fatalf("binary: %v", err)
	}
	// protocol violations
	if _, cl, err = relay(t, block, clientFrame(true, opContinuation, []byte("x"))); !errors.Is(err, errClosed) || !bytes.Equal(cl, closeFrame(1002)) {
		t.Errorf("orphan continuation: %v", err)
	}
	if _, cl, err = relay(t, block, f1, clientFrame(true, opText, []byte("y"))); !errors.Is(err, errClosed) || !bytes.Equal(cl, closeFrame(1002)) {
		t.Errorf("new message inside a fragmented one: %v", err)
	}
	if _, cl, err = relay(t, block, clientFrame(false, 0x9, []byte("x"))); !errors.Is(err, errClosed) || !bytes.Equal(cl, closeFrame(1002)) {
		t.Errorf("fragmented control frame: %v", err)
	}
	if _, cl, err = relay(t, block, clientFrame(true, 0x9, bytes.Repeat([]byte("x"), 126))); !errors.Is(err, errClosed) || !bytes.Equal(cl, closeFrame(1002)) {
		t.Errorf("long control frame: %v", err)
	}
	if _, cl, err = relay(t, block, clientFrame(true, opText, bytes.Repeat([]byte("x"), maxTextMessage+1))); !errors.Is(err, errClosed) || !bytes.Equal(cl, closeFrame(1009)) {
		t.Errorf("too big: %v", err)
	}
	// truncated input ends with an I/O error, not a panic
	if _, _, err = relay(t, block, text0()[:3]); err == nil {
		t.Error("truncated")
	}
	if _, _, err = relay(t, block, text0()[:2]); err == nil {
		t.Error("truncated payload")
	}
	// 64-bit length that is absurd
	absurd := []byte{0x81, 127, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	if _, _, err = relay(t, block, absurd); err == nil {
		t.Error("absurd length")
	}
}

func text0() []byte { return clientFrame(true, opText, []byte("hello")) }

// handshakeUpstream is a raw TCP upstream: it answers the upgrade and records what the client sent after it.
type wsUpstream struct {
	ln       net.Listener
	request  chan string
	received chan []byte
}

func newWSUpstream(t *testing.T) *wsUpstream {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &wsUpstream{ln: ln, request: make(chan string, 1), received: make(chan []byte, 16)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		u.request <- req.Header.Get("Cookie") + "|" + req.Header.Get("X-Afs-Authorization") + "|" + req.Header.Get("X-Forwarded-For") + "|" + req.URL.RequestURI() + "|" + req.Host
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = c.Write([]byte{0x81, 0x02, 'h', 'i'})
		buf := make([]byte, 4096)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				u.received <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	return u
}

func wsEdge(t *testing.T, u *wsUpstream, st *stub, idle time.Duration) string {
	t.Helper()
	srv := New(Config{Service: st, IdleTimeout: idle,
		Dial: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, u.ln.Addr().String())
		}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "http://")
}

const upgradeReq = "GET /socket?a=1 HTTP/1.1\r\nHost: alpha.collab.example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nCookie: __Host-afs-collab=s; p=1\r\nX-Afs-Authorization: Bearer t\r\nX-Forwarded-For: 6.6.6.6\r\n\r\n"

func dialEdge(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, upgradeReq); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	return c, br
}

func TestWebSocketTunnel(t *testing.T) {
	const key = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // the documented AWS example key, not a credential
	for _, c := range []struct {
		name      string
		frames    bool
		send      []byte
		wantClose bool
		wantRecv  bool
	}{
		{"contributor clean text", true, clientFrame(true, opText, []byte("hello")), false, true},
		{"contributor secret", true, clientFrame(true, opText, []byte(key)), true, false},
		{"watcher text", false, clientFrame(true, opText, []byte("hello")), true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newWSUpstream(t)
			st := &stub{block: true, frames: c.frames, res: edge.Result{Action: edge.Forward, Upstream: "http://paperclip.team-a.svc.cluster.local:3100", Status: 200,
				ScanMode: "block", Route: collab.Route{TeamID: "team_01"}}}
			conn, br := dialEdge(t, wsEdge(t, u, st, time.Minute))
			// the upstream saw the sanitized handshake
			select {
			case got := <-u.request:
				if got != "p=1|||/socket?a=1|alpha.collab.example.com" && !strings.HasPrefix(got, "p=1||127.0.0.1|/socket?a=1|") {
					t.Fatalf("handshake at upstream: %q", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no handshake")
			}
			// server-to-client bytes arrive untouched
			hi := make([]byte, 4)
			if _, err := io.ReadFull(br, hi); err != nil || string(hi[2:]) != "hi" {
				t.Fatalf("server frame: %v %v", hi, err)
			}
			if _, err := conn.Write(c.send); err != nil {
				t.Fatal(err)
			}
			if c.wantClose {
				cf := make([]byte, 4)
				if _, err := io.ReadFull(br, cf); err != nil || !bytes.Equal(cf, closeFrame(1008)) {
					t.Fatalf("close frame: %v %v", cf, err)
				}
				select {
				case b := <-u.received:
					t.Fatalf("refused frame reached the upstream: %v", b)
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			select {
			case b := <-u.received:
				if !bytes.Equal(b, c.send) {
					t.Fatalf("frame changed: %v", b)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("frame not forwarded")
			}
		})
	}
}

func TestWebSocketIdleTimeout(t *testing.T) {
	u := newWSUpstream(t)
	st := &stub{frames: true, res: edge.Result{Action: edge.Forward, Upstream: "http://paperclip.team-a.svc.cluster.local:3100", Status: 200, Route: collab.Route{TeamID: "team_01"}}}
	conn, br := dialEdge(t, wsEdge(t, u, st, 150*time.Millisecond))
	hi := make([]byte, 4)
	_, _ = io.ReadFull(br, hi)
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.Copy(io.Discard, br); err != nil && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("an idle connection must end, not time out here: %v", err)
	}
}

func TestWebSocketUpstreamDown(t *testing.T) {
	u := newWSUpstream(t)
	_ = u.ln.Close()
	st := &stub{frames: true, res: edge.Result{Action: edge.Forward, Upstream: "http://paperclip.team-a.svc.cluster.local:3100", Status: 200, Route: collab.Route{TeamID: "team_01"}}}
	addr := wsEdge(t, u, st, time.Minute)
	c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, upgradeReq)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("%v %v", resp, err)
	}
}
