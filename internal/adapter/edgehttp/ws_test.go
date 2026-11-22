package edgehttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
	// binary that is valid UTF-8 is scanned like text; other binary passes
	if _, cl, err = relay(t, block, clientFrame(true, opBinary, []byte("SECRET"))); !errors.Is(err, errClosed) || !bytes.Equal(cl, closeFrame(1008)) {
		t.Fatalf("binary secret: %v", err)
	}
	if _, _, err = relay(t, block, clientFrame(true, opBinary, []byte{0xff, 0xfe, 'S', 'E', 'C', 'R', 'E', 'T'})); !errors.Is(err, io.EOF) {
		t.Fatalf("binary file: %v", err)
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
	// test knobs: a wrong accept value, a non-101 status, extra response header lines
	badAccept bool
	status    int
	extra     string
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
		accept := acceptKey(req.Header.Get("Sec-WebSocket-Key"))
		if u.badAccept {
			accept = "AAAAAAAAAAAAAAAAAAAAAAAAAAA="
		}
		u.request <- req.Header.Get("Cookie") + "|" + req.Header.Get("X-Afs-Authorization") + "|" + req.Header.Get("X-Forwarded-For") + "|" + req.URL.RequestURI() + "|" + req.Host
		if u.status != 0 {
			_, _ = fmt.Fprintf(c, "HTTP/1.1 %d X\r\nSet-Cookie: a=b; Domain=collab.example.com\r\nContent-Length: 4\r\nConnection: close\r\n\r\nnope", u.status)
			return
		}
		_, _ = fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n%s\r\n", accept, u.extra)
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

// Finding 3: the scan cannot be skipped with extensions, reserved bits or reserved opcodes, and fragmented
// binary messages are scanned on reassembly.
func TestRelayScanBypasses(t *testing.T) {
	block := framePolicy{CanSend: true, Scan: func(b []byte) bool { return bytes.Contains(b, []byte("SECRET")) }}
	withBits := func(f []byte, bits byte) []byte { f[0] |= bits; return f }
	violations := map[string][]byte{
		"RSV1 (permessage-deflate)": withBits(clientFrame(true, opText, []byte("x")), 0x40),
		"RSV2":                      withBits(clientFrame(true, opText, []byte("x")), 0x20),
		"RSV3":                      withBits(clientFrame(true, opBinary, []byte("x")), 0x10),
		"RSV on continuation":       withBits(clientFrame(true, opContinuation, []byte("x")), 0x40),
		"RSV on control":            withBits(clientFrame(true, 0x9, nil), 0x40),
		"unmasked":                  append([]byte{0x81, 0x01}, 'x'),
	}
	for op := byte(3); op <= 7; op++ {
		violations[fmt.Sprintf("reserved data opcode %d", op)] = clientFrame(true, op, []byte("SECRET"))
	}
	for op := byte(0xB); op <= 0xF; op++ {
		violations[fmt.Sprintf("reserved control opcode %#x", op)] = clientFrame(true, op, nil)
	}
	for name, f := range violations {
		if fw, cl, err := relay(t, block, f); !errors.Is(err, errClosed) || !bytes.Equal(cl, closeFrame(1002)) || len(fw) != 0 {
			t.Errorf("%s: %v %v", name, err, cl)
		}
	}
	a, b := clientFrame(false, opBinary, []byte("xxSEC")), clientFrame(true, opContinuation, []byte("RETxx"))
	if fw, cl, err := relay(t, block, a, b); !errors.Is(err, errClosed) || len(fw) != 0 || !bytes.Equal(cl, closeFrame(1008)) {
		t.Errorf("fragmented binary secret: %v", err)
	}
	// a ping between fragments does not reset the scan
	c := clientFrame(true, 0x9, []byte("p"))
	if _, _, err := relay(t, block, a, c, b); !errors.Is(err, errClosed) {
		t.Errorf("secret around a ping: %v", err)
	}
	if _, _, err := relay(t, block, clientFrame(true, opBinary, bytes.Repeat([]byte("b"), maxTextMessage+1))); !errors.Is(err, errClosed) {
		t.Error("large binary message must close with 1009")
	}
}

func TestScanMessageUsedByTunnel(t *testing.T) {
	if len(collab.ScanMessage([]byte(`{"k":"AKIA\u0041BCDEFGHIJKLMNOP"}`))) == 0 {
		t.Error("escaped secret in a message")
	}
}

// Finding 5: the upstream's answer is validated before any byte is tunnelled.
func TestWebSocketHandshakeValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		up      wsUpstream
		want    int
		cookie  bool
		keepsWS bool
	}{
		{"wrong accept", wsUpstream{badAccept: true}, 503, false, false},
		{"upstream refuses", wsUpstream{status: 403}, 403, true, false},
		{"upstream 200 instead of 101", wsUpstream{status: 200}, 200, true, false},
		{"extension answered", wsUpstream{extra: "Sec-WebSocket-Extensions: permessage-deflate\r\n"}, 503, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newWSUpstream(t)
			u.badAccept, u.status, u.extra = tc.up.badAccept, tc.up.status, tc.up.extra
			addr := wsEdge(t, u, &stub{res: edge.Result{Action: edge.Forward, Upstream: "http://x.svc:3100", Status: 200, ScanMode: "block"}, frames: true}, time.Minute)
			c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = io.WriteString(c, upgradeReq)
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want || resp.StatusCode == 101 {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
			for _, sc := range resp.Header.Values("Set-Cookie") {
				if strings.Contains(strings.ToLower(sc), "domain") {
					t.Errorf("relayed refusal keeps Domain: %q", sc)
				}
			}
			if tc.cookie && len(resp.Header.Values("Set-Cookie")) == 0 {
				t.Error("the refusal's cookie is relayed (without Domain)")
			}
			// nothing is tunnelled afterwards: the edge closes
			_, _ = c.Write(clientFrame(true, opText, []byte("hello")))
			body, _ := io.ReadAll(resp.Body)
			if tc.want == 503 && !strings.Contains(string(body), `"code":"Unavailable"`) {
				t.Errorf("want the AFS error envelope, got %q", body)
			}
			if tc.want != 503 && string(body) != "nope" {
				t.Errorf("relayed body %q", body)
			}
			select {
			case got := <-u.received:
				t.Errorf("upstream received %q", got)
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

func TestWebSocketRejectsBadClientKey(t *testing.T) {
	u := newWSUpstream(t)
	addr := wsEdge(t, u, &stub{res: edge.Result{Action: edge.Forward, Upstream: "http://x.svc:3100", Status: 200}}, time.Minute)
	c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, strings.Replace(upgradeReq, "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Key: short", 1))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 400 {
		t.Fatalf("%v %v", resp, err)
	}
	_ = resp.Body.Close()
}

// Finding 3: the extension header never reaches the upstream.
func TestWebSocketExtensionsStripped(t *testing.T) {
	h := http.Header{}
	h.Set("Sec-WebSocket-Extensions", "permessage-deflate; client_max_window_bits")
	h.Set("X-HTTP-Method-Override", "DELETE")
	h.Set("X-Http-Method", "PUT")
	h.Set("X-Method-Override", "PATCH")
	sanitize(h)
	for _, k := range []string{"Sec-WebSocket-Extensions", "X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"} {
		if h.Get(k) != "" {
			t.Errorf("%s forwarded", k)
		}
	}
}

// fakeClock drives the re-authorization.
type fakeClock struct {
	c    chan time.Time
	stop chan struct{}
	d    time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{c: make(chan time.Time), stop: make(chan struct{})} }

func (f *fakeClock) ticker(d time.Duration) (<-chan time.Time, func()) {
	f.d = d
	return f.c, func() {
		select {
		case <-f.stop:
		default:
			close(f.stop)
		}
	}
}

func (f *fakeClock) tick(t *testing.T) {
	t.Helper()
	select {
	case f.c <- time.Now():
	case <-time.After(3 * time.Second):
		t.Fatal("no ticker is waiting")
	}
}

// Finding 2: an open WebSocket is closed with 1008 once the check fails.
func TestWebSocketReauthorized(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func() edge.Result
		close bool
	}{
		{"still allowed", nil, false},
		{"session ended", func() edge.Result { return edge.Result{Action: edge.Respond, Status: 401} }, true},
		{"binding removed", func() edge.Result { return edge.Result{Action: edge.Respond, Status: 403} }, true},
		{"surface off", func() edge.Result { return edge.Result{Action: edge.Respond, Status: 404} }, true},
		{"scan mode changed", func() edge.Result {
			return edge.Result{Action: edge.Forward, Upstream: "http://x.svc:3100", Status: 200, ScanMode: "warn"}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newWSUpstream(t)
			clock := newFakeClock()
			st := &stub{res: edge.Result{Action: edge.Forward, Upstream: "http://x.svc:3100", Status: 200, ScanMode: "block"}, frames: true, recheck: tc.after}
			srv := New(Config{Service: st, RecheckEvery: time.Minute, NewTicker: clock.ticker,
				Dial: func(ctx context.Context, n, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, n, u.ln.Addr().String())
				}})
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			c, br := dialEdge(t, strings.TrimPrefix(ts.URL, "http://"))
			_, _ = br.Discard(4) // the upstream's first frame
			clock.tick(t)
			if clock.d != time.Minute {
				t.Errorf("interval %v", clock.d)
			}
			if !tc.close {
				// still open: a client frame reaches the upstream
				_, _ = c.Write(clientFrame(true, opText, []byte("hello")))
				select {
				case <-u.received:
				case <-time.After(2 * time.Second):
					t.Fatal("stream was closed")
				}
				return
			}
			hdr := make([]byte, 4)
			if _, err := io.ReadFull(br, hdr); err != nil || !bytes.Equal(hdr, closeFrame(1008)) {
				t.Fatalf("want a 1008 close frame, got %v %v", hdr, err)
			}
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := br.ReadByte(); err == nil {
				t.Error("connection must be closed after the close frame")
			}
		})
	}
}

func TestEventStreamReauthorized(t *testing.T) {
	released := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Set-Cookie", "sid=1; Domain=collab.example.com; Path=/")
		w.Header().Add("Set-Cookie", "__Host-afs-collab=evil; Path=/")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: 1\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer up.Close()
	defer close(released)
	clock := newFakeClock()
	st := &stub{res: edge.Result{Action: edge.Forward, Upstream: up.URL, Status: 200}, recheck: func() edge.Result {
		return edge.Result{Action: edge.Respond, Status: 401}
	}}
	srv := New(Config{Service: st, RecheckEvery: time.Minute, NewTicker: clock.ticker})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/events") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Values("Set-Cookie"); len(got) != 1 || strings.Contains(strings.ToLower(got[0]), "domain") {
		t.Errorf("Set-Cookie = %q", got)
	}
	br := bufio.NewReader(resp.Body)
	if line, _ := br.ReadString('\n'); line != "data: 1\n" {
		t.Fatalf("first event %q", line)
	}
	clock.tick(t)
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, br); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("event stream still open after a failed re-check")
	}
}
