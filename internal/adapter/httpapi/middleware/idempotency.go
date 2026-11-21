package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Idempotency-Key handling (API contract §1, database contract §7.1).
//
// The key is claimed in the mutation's own transaction, so a 5xx or any other rollback removes it and
// a concurrent duplicate waits on the claim (or is told it is in progress). That needs the use case's
// transaction, which only the use case opens, so the work is split in two:
//
//   - Idempotency (a PostAuth middleware) validates the header, hashes the request, buffers the
//     response and, after the handler returns, stores a 2xx response against the claim and replays a
//     stored one;
//   - IdempotentRunner wraps the application's port.TxRunner: the first InTx of a request that carries
//     a key begins the claim inside that transaction.
//
// Every use case of a creating POST or :verb action must therefore be built on the IdempotentRunner
// and open exactly one InTx for its mutation. A request whose handler never used it is answered
// normally but logged, because its key was not honored.

const (
	maxKeyLength = 255
	// maxStoredResponse bounds the response kept for replay.
	maxStoredResponse = 1 << 20
	completeAttempts  = 3
)

type claim struct {
	key port.IdempotencyKey

	mu     sync.Mutex
	taken  bool
	begun  bool
	replay *port.StoredResponse
}

func (c *claim) take() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.taken {
		return false
	}
	c.taken = true
	return true
}

func (c *claim) set(begun bool, replay *port.StoredResponse) {
	c.mu.Lock()
	c.begun, c.replay = begun, replay
	c.mu.Unlock()
}

func (c *claim) state() (taken, begun bool, replay *port.StoredResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.taken, c.begun, c.replay
}

// errReplay aborts the transaction of a request whose key already has a stored response. The
// middleware discards whatever the handler then wrote and replays the stored response.
var errReplay = domain.Conflict("idempotent_replay", "a stored response is replayed", nil)

// Idempotency implements the Idempotency-Key rules as a middleware and a transaction wrapper.
type Idempotency struct {
	Store  port.IdempotencyStore
	Runner port.TxRunner
	// Session returns the database session of the authenticated caller, for the transaction that
	// stores the response.
	Session func(ctx context.Context) (port.Session, bool)
	Routes  RouteLookup
	Errors  ErrorWriter
	Log     *slog.Logger
	// Backoff is the wait between attempts to store a response; default 50 ms.
	Backoff time.Duration
}

func (i *Idempotency) log() *slog.Logger {
	if i.Log != nil {
		return i.Log
	}
	return slog.Default()
}

// WrapRunner wraps inner so the first InTx of a request carrying a key claims it. Use cases must receive
// this runner, not inner.
func (i *Idempotency) WrapRunner(inner port.TxRunner) port.TxRunner {
	return &IdempotentRunner{Inner: inner, Store: i.Store}
}

// IdempotentRunner is a port.TxRunner that claims the request's idempotency key in the first
// read-write transaction it opens for that request.
type IdempotentRunner struct {
	Inner port.TxRunner
	Store port.IdempotencyStore
}

// InTx implements port.TxRunner.
func (r *IdempotentRunner) InTx(ctx context.Context, s port.Session, fn func(context.Context, port.Tx) error) error {
	c, _ := ctx.Value(keyClaim).(*claim)
	if c == nil || !c.take() {
		return r.Inner.InTx(ctx, s, fn)
	}
	return r.Inner.InTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
		stored, err := r.Store.Begin(ctx, tx, c.key)
		if err != nil {
			return err
		}
		if stored != nil {
			c.set(false, stored)
			return errReplay
		}
		c.set(true, nil)
		return fn(ctx, tx)
	})
}

// ReadTx implements port.TxRunner; reads never claim a key.
func (r *IdempotentRunner) ReadTx(ctx context.Context, s port.Session, fn func(context.Context, port.Tx) error) error {
	return r.Inner.ReadTx(ctx, s, fn)
}

// Middleware is the PostAuth part. It runs after authorization, so only a caller allowed to make the
// request can touch the store, and the key is scoped to that caller by the session of the transaction.
func (i *Idempotency) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info, ok := i.Routes(r)
			if !ok || !info.Idempotent {
				next.ServeHTTP(w, r)
				return
			}
			keys := r.Header.Values("Idempotency-Key")
			if len(keys) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			key, err := validKey(keys)
			if err != nil {
				i.Errors(w, r, err)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				i.Errors(w, r, domain.Validation(domain.FieldError{Field: "body", Reason: "request body is larger than the limit or unreadable"}))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			h := sha256.New()
			h.Write([]byte(r.Method + " " + r.URL.EscapedPath() + "\n"))
			h.Write(body)
			c := &claim{key: port.IdempotencyKey{Key: key, Operation: info.OperationID, TeamID: r.PathValue("teamId")}}
			copy(c.key.RequestSHA256[:], h.Sum(nil))

			bw := &bufferedWriter{ResponseWriter: w, limit: maxStoredResponse}
			next.ServeHTTP(bw, r.WithContext(context.WithValue(r.Context(), keyClaim, c)))
			i.finish(w, r, bw, c, info)
		})
	}
}

func validKey(keys []string) (string, error) {
	bad := func(reason string) error {
		return domain.Validation(domain.FieldError{Field: "Idempotency-Key", Reason: reason})
	}
	if len(keys) != 1 {
		return "", bad("send one Idempotency-Key header")
	}
	k := keys[0]
	if k == "" || len(k) > maxKeyLength {
		return "", bad("must be 1 to 255 characters")
	}
	for j := 0; j < len(k); j++ {
		if k[j] < 0x21 || k[j] > 0x7e {
			return "", bad("must be printable ASCII without spaces")
		}
	}
	return k, nil
}

func (i *Idempotency) finish(w http.ResponseWriter, r *http.Request, bw *bufferedWriter, c *claim, info RouteInfo) {
	taken, begun, replay := c.state()
	if replay != nil {
		// The handler's response (an abort caused by the replay) is dropped.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(replay.Status)
		_, _ = w.Write(replay.Body)
		return
	}
	switch {
	case bw.passthrough:
		// A response too large to buffer streamed out; it cannot be stored.
		if begun {
			i.log().ErrorContext(r.Context(), "idempotent response too large to store", "op", info.OperationID)
		}
	case begun && bw.status/100 == 2:
		i.store(r.Context(), bw, c)
	case begun && bw.status >= 500:
		i.log().ErrorContext(r.Context(), "5xx after the idempotency claim committed; the key stays until it expires", "op", info.OperationID, "status", bw.status)
	case !taken && bw.status/100 == 2:
		i.log().ErrorContext(r.Context(), "Idempotency-Key not honored: the handler did not open its transaction through the IdempotentRunner", "op", info.OperationID)
	}
	bw.finish(nil)
}

// store records the response against the claim, in its own transaction as the same caller.
func (i *Idempotency) store(ctx context.Context, bw *bufferedWriter, c *claim) {
	s, ok := i.Session(ctx)
	if !ok {
		i.log().ErrorContext(ctx, "idempotent response not stored: no authenticated caller")
		return
	}
	ctx = context.WithoutCancel(ctx) // the mutation committed; storing its response must not be cut short
	resp := port.StoredResponse{Status: bw.status, Body: bw.body.Bytes()}
	var err error
	for attempt := 0; attempt < completeAttempts; attempt++ {
		err = i.Runner.InTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
			return i.Store.Complete(ctx, tx, c.key, resp)
		})
		if err == nil {
			return
		}
		backoff := i.Backoff
		if backoff == 0 {
			backoff = 50 * time.Millisecond
		}
		time.Sleep(backoff)
	}
	i.log().ErrorContext(ctx, "idempotent response not stored; a retry gets idempotency_in_progress until the key expires", "err", err)
}
