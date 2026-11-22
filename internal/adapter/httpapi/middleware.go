package httpapi

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyPrincipal
	keyRoute
)

// RequestIDFrom returns the request id of ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(keyRequestID).(string)
	return s
}

// withRequestID accepts a client X-Request-Id of 1–64 printable characters and otherwise
// generates one (an invalid id is replaced, not refused), and stamps every response, error
// responses included, with X-Request-Id and X-Afs-Contract-Version.
func withRequestID(now func() time.Time, entropy io.Reader, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := string(domain.AcceptRequestID(r.Header.Get("X-Request-Id"), now(), entropy))
		w.Header().Set("X-Request-Id", rid)
		w.Header().Set("X-Afs-Contract-Version", api.ContractVersion)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyRequestID, rid)))
	})
}

// withRecovery turns a panic into a 500 envelope and logs the stack.
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared as net/http does
					panic(v)
				}
				slog.ErrorContext(r.Context(), "panic", "requestId", RequestIDFrom(r.Context()), "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				WriteError(w, r, domain.Internal(fmt.Errorf("panic: %v", v)))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func defaultEntropy() io.Reader { return rand.Reader }
