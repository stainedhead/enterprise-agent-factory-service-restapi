// Package mcpbridge runs a stdio MCP server as a child process and serves it as a Streamable HTTP
// endpoint for afs-gateway (PRD §10.2, story MCP-RA-7). Requests are serialised, because a stdio server
// handles one client stream; the child is restarted when it exits, and the last initialize is replayed
// so a 2025-11-25 session survives the restart.
package mcpbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
)

// Config configures a Bridge.
type Config struct {
	Command    []string      // the server command and its arguments
	Env        []string      // the child's environment (KEY=value); nothing is inherited
	Dir        string        // working directory
	Timeout    time.Duration // per request; default 60 s
	MaxRequest int64         // bytes; default 1 MiB
	Backoff    time.Duration // first restart delay, doubled up to 30 s; default 500 ms
	Log        *slog.Logger
}

// Bridge is an http.Handler for one stdio server.
type Bridge struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex // serialises requests
	stateMu  sync.Mutex
	stdin    io.WriteCloser
	running  bool
	pending  chan *mcpwire.Message // the one in-flight response
	waitID   string
	seq      int64
	initReq  *mcpwire.Message // last successful initialize, replayed after a restart
	restarts int
	ready    chan struct{}
}

// New returns a Bridge. Call Run to start the child.
func New(cfg Config) (*Bridge, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("mcpbridge: command is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxRequest <= 0 {
		cfg.MaxRequest = 1 << 20
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 500 * time.Millisecond
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Bridge{cfg: cfg, log: cfg.Log, ready: make(chan struct{})}, nil
}

// Run starts the child and restarts it until ctx ends.
func (b *Bridge) Run(ctx context.Context) error {
	delay := b.cfg.Backoff
	for {
		started := time.Now()
		err := b.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		b.log.Warn("mcp server exited; restarting", "err", err, "after", delay)
		if time.Since(started) > 30*time.Second {
			delay = b.cfg.Backoff
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (b *Bridge) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, b.cfg.Command[0], b.cfg.Command[1:]...) //nolint:gosec // the command is the hosted server's own, from its pinned spec
	cmd.Env = b.cfg.Env
	cmd.Dir = b.cfg.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = &logWriter{log: b.log}
	if err := cmd.Start(); err != nil {
		return err
	}
	b.stateMu.Lock()
	b.stdin, b.running = stdin, true
	replay := b.initReq
	first := b.restarts == 0
	b.restarts++
	b.stateMu.Unlock()
	if first {
		close(b.ready)
	}
	done := make(chan struct{})
	go func() { b.readLoop(stdout); close(done) }()
	if replay != nil {
		go b.replayInit(ctx, replay)
	}
	err = cmd.Wait()
	<-done
	b.stateMu.Lock()
	b.running = false
	if b.pending != nil {
		b.pending <- mcpwire.NewError(nil, mcpwire.CodeInternalError, "mcp server restarted", map[string]string{"reason": "server_restarted"})
		b.pending = nil
	}
	b.stateMu.Unlock()
	return err
}

func (b *Bridge) replayInit(ctx context.Context, init *mcpwire.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.roundTrip(ctx, init, false); err != nil {
		b.log.Warn("initialize replay failed", "err", err)
		return
	}
	_ = b.write(&mcpwire.Message{JSONRPC: "2.0", Method: "notifications/initialized"})
}

// readLoop reads newline-delimited messages from the child.
func (b *Bridge) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		m, perr := mcpwire.Parse(sc.Bytes())
		if perr != nil {
			b.log.Warn("mcp server wrote an invalid message", "err", perr.Message)
			continue
		}
		switch {
		case m.IsResponse():
			b.stateMu.Lock()
			if b.pending != nil && string(m.ID) == b.waitID {
				b.pending <- m
				b.pending = nil
			}
			b.stateMu.Unlock()
		case m.IsRequest():
			// Sampling, elicitation and roots are not offered through the bridge (gateway contract §13.7).
			_ = b.write(mcpwire.NewError(m.ID, mcpwire.CodeMethodNotFound, "not supported by afs-mcp-bridge", nil))
		}
		// Notifications from a stdio server are dropped: the gateway refreshes from list calls.
	}
}

func (b *Bridge) write(m *mcpwire.Message) error {
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b.stateMu.Lock()
	w, ok := b.stdin, b.running
	b.stateMu.Unlock()
	if !ok {
		return errNotRunning
	}
	_, err = w.Write(append(line, '\n'))
	return err
}

var errNotRunning = errors.New("mcpbridge: server not running")

// roundTrip sends one request with a bridge-owned id and waits for its response. The caller holds mu.
func (b *Bridge) roundTrip(ctx context.Context, m *mcpwire.Message, remember bool) (*mcpwire.Message, error) {
	b.seq++
	id := strconv.FormatInt(b.seq, 10)
	ch := make(chan *mcpwire.Message, 1)
	b.stateMu.Lock()
	b.pending, b.waitID = ch, id
	b.stateMu.Unlock()
	out := *m
	out.ID = json.RawMessage(id)
	if err := b.write(&out); err != nil {
		b.clearPending()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	select {
	case resp := <-ch:
		resp.ID = m.ID
		if remember && m.Method == "initialize" && resp.Error == nil {
			b.stateMu.Lock()
			b.initReq = m
			b.stateMu.Unlock()
		}
		return resp, nil
	case <-ctx.Done():
		b.clearPending()
		_ = b.write(&mcpwire.Message{JSONRPC: "2.0", Method: "notifications/cancelled",
			Params: json.RawMessage(`{"requestId":` + id + `,"reason":"timeout"}`)})
		return nil, ctx.Err()
	}
}

func (b *Bridge) clearPending() {
	b.stateMu.Lock()
	b.pending = nil
	b.stateMu.Unlock()
}

// Ready is closed once the child has started the first time.
func (b *Bridge) Ready() <-chan struct{} { return b.ready }

// Running reports whether the child is running.
func (b *Bridge) Running() bool {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.running
}

// ServeHTTP serves POST /mcp and GET /healthz.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		if !b.Running() {
			http.Error(w, "server not running", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
		return
	case r.URL.Path != "/mcp":
		http.NotFound(w, r)
		return
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, b.cfg.MaxRequest))
	if err != nil {
		reply(w, http.StatusRequestEntityTooLarge, mcpwire.NewError(nil, mcpwire.CodeInvalidRequest, "request too large", nil))
		return
	}
	m, perr := mcpwire.Parse(body)
	if perr != nil {
		reply(w, http.StatusBadRequest, &mcpwire.Message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: perr})
		return
	}
	if herr := mcpwire.CheckHeaders(r.Header, m); herr != nil {
		reply(w, http.StatusBadRequest, &mcpwire.Message{JSONRPC: "2.0", ID: idOrNull(m.ID), Error: herr})
		return
	}
	if !m.IsRequest() {
		if m.IsNotification() {
			b.mu.Lock()
			err := b.write(m)
			b.mu.Unlock()
			if err != nil {
				http.Error(w, "server not running", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	b.mu.Lock()
	resp, err := b.roundTrip(r.Context(), m, true)
	b.mu.Unlock()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reply(w, http.StatusGatewayTimeout, mcpwire.NewError(m.ID, mcpwire.CodeTimeout, "mcp server timed out", map[string]string{"reason": "timeout"}))
	case err != nil:
		reply(w, http.StatusServiceUnavailable, mcpwire.NewError(m.ID, mcpwire.CodeServerError, "mcp server unavailable", map[string]string{"reason": "upstream_unavailable"}))
	default:
		reply(w, http.StatusOK, resp)
	}
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func reply(w http.ResponseWriter, status int, m *mcpwire.Message) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(m)
}

// logWriter forwards the child's stderr to the log, line by line, capped per line.
type logWriter struct {
	log *slog.Logger
	buf []byte
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := -1
		for j, c := range l.buf {
			if c == '\n' {
				i = j
				break
			}
		}
		if i < 0 {
			if len(l.buf) > 4096 {
				l.log.Info("mcp server stderr", "line", string(l.buf[:4096]))
				l.buf = l.buf[:0]
			}
			return len(p), nil
		}
		line := l.buf[:i]
		if len(line) > 4096 {
			line = line[:4096]
		}
		l.log.Info("mcp server stderr", "line", string(line))
		l.buf = l.buf[i+1:]
	}
}
