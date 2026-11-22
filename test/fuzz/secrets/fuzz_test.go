// Package secrets_test is the secret-value fuzz test of M2-C4 (RA-US-5.3, SEC-4). It drives every
// secret route, success and failure paths, with fuzzed values and asserts that the value never
// appears in a response body or header, a log line, a trace attribute, an audit record or an event.
// The handler is the real httpapi router with the real secrets use case on in-memory ports.
package secrets_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets/secretstest"
)

const (
	team   = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	agent  = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	marker = "FUZZVALUE-9c41e7d2-"
)

type auth struct{}

func (auth) Authenticate(r *http.Request) (*authz.Principal, error) {
	switch r.Header.Get("X-Test-Role") {
	case "admin":
		return &authz.Principal{ID: "usr_01JD2Q3R4S5T6V7W8X9Y0Z0009", Type: authz.PrincipalUser,
			Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}, nil
	case "viewer":
		return &authz.Principal{ID: "usr_01JD2Q3R4S5T6V7W8X9Y0Z0008", Type: authz.PrincipalUser,
			Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: team}}}, nil
	}
	return nil, nil
}

// logCapture is a slog handler that keeps every record, with attributes, as text.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (l *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logCapture) WithGroup(string) slog.Handler            { return l }
func (l *logCapture) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, "%s", r.Message)
	r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&l.buf, " %s=%v", a.Key, a.Value.Resolve()); return true })
	l.buf.WriteByte('\n')
	return nil
}

type rig struct {
	h      http.Handler
	w      *secretstest.Writer
	audit  *fakes.Audit
	outbox *fakes.Outbox
	logs   *logCapture
	spans  *tracetest.SpanRecorder
	seen   strings.Builder // every response, with headers
}

func newRig(t testing.TB) *rig {
	t.Helper()
	r := &rig{w: secretstest.NewWriter(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}, logs: &logCapture{}, spans: tracetest.NewSpanRecorder()}
	repo := secretstest.NewRepo()
	repo.Agents[agent] = "coder"
	svc := &secrets.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: repo, Writer: r.w}
	api := &httpapi.SecretsAPI{Svc: svc, Pages: httpapi.NewPageTokens(bytes.Repeat([]byte("k"), 32), nil)}
	h, err := httpapi.New(httpapi.Config{Auth: auth{}, Registrars: []httpapi.Registrar{api.Register}})
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	return r
}

func (r *rig) do(method, path, role, body string, hdr ...string) (int, string) {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	req.Header.Set("X-Test-Role", role)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	req.RemoteAddr = "198.51.100.7:4000"
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	fmt.Fprintf(&r.seen, "%d\n", rec.Code)
	for k, v := range rec.Header() {
		fmt.Fprintf(&r.seen, "%s: %s\n", k, strings.Join(v, ","))
	}
	r.seen.Write(b)
	r.seen.WriteByte('\n')
	return rec.Code, string(b)
}

func jsonBody(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }

// exercise sends the value through every secret route, on success and failure paths.
func (r *rig) exercise(t testing.TB, value string) {
	t.Helper()
	base := "/v1/teams/" + team + "/secrets"
	val := map[string]any{"value": value}
	expect := func(got, want int, what string) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: status %d, want %d", what, got, want)
		}
	}
	// Create, replay, duplicate, invalid.
	create := jsonBody(map[string]any{"scope": "team", "name": "fuzz-one", "value": value})
	code, body := r.do("POST", base, "admin", create, "Idempotency-Key", "k-1")
	expect(code, 201, "create")
	var sec struct{ ID string }
	_ = json.Unmarshal([]byte(body), &sec)
	code, _ = r.do("POST", base, "admin", create, "Idempotency-Key", "k-1")
	expect(code, 201, "create replay")
	code, _ = r.do("POST", base, "admin", create)
	expect(code, 409, "duplicate")
	code, _ = r.do("POST", base, "admin", jsonBody(map[string]any{"scope": "team", "name": "Bad Name", "value": value}))
	expect(code, 400, "bad name")
	code, _ = r.do("POST", base, "admin", jsonBody(map[string]any{"scope": "agent", "agentId": "agt_01JD2Q3R4S5T6V7W8X9Y0Z0009", "name": "fuzz-two", "value": value}))
	expect(code, 400, "unknown agent")
	code, _ = r.do("POST", base, "admin", jsonBody(map[string]any{"scope": "agent", "agentId": agent, "name": "fuzz-agent", "value": value}))
	expect(code, 201, "agent scope")
	code, _ = r.do("POST", base, "viewer", create)
	expect(code, 403, "viewer create")
	code, _ = r.do("POST", base, "", create)
	expect(code, 401, "anonymous create")
	// A body with the value and a field the contract does not have, and bodies cut off mid-value.
	code, _ = r.do("POST", base, "admin", jsonBody(map[string]any{"scope": "team", "name": "x", "value": value, "extra": 1}))
	expect(code, 400, "unknown field")
	for _, op := range []struct{ m, p string }{{"POST", base}, {"PUT", base + "/" + sec.ID + "/value"}, {"POST", base + "/" + sec.ID + ":rotate"}} {
		whole := jsonBody(map[string]any{"scope": "team", "name": "cut", "value": value})
		cut := whole[:len(whole)-2]
		code, _ = r.do(op.m, op.p, "admin", cut)
		expect(code, 400, "truncated body "+op.p)
		code, _ = r.do(op.m, op.p, "admin", `{"value":`+jsonBody(map[string]any{"v": value})+`}`)
		expect(code, 400, "value of the wrong type "+op.p)
	}
	// Too large.
	code, _ = r.do("PUT", base+"/"+sec.ID+"/value", "admin", jsonBody(map[string]any{"value": value + strings.Repeat("x", 70000)}))
	expect(code, 400, "over 64 KiB")
	// Put and rotate, success, stale version, replay.
	code, _ = r.do("PUT", base+"/"+sec.ID+"/value", "admin", jsonBody(val))
	expect(code, 204, "put")
	code, _ = r.do("PUT", base+"/"+sec.ID+"/value", "admin", jsonBody(val), "If-Match", `"1"`)
	expect(code, 412, "put stale")
	rot := jsonBody(val)
	code, _ = r.do("POST", base+"/"+sec.ID+":rotate", "admin", rot, "Idempotency-Key", "k-2")
	expect(code, 200, "rotate")
	code, _ = r.do("POST", base+"/"+sec.ID+":rotate", "admin", rot, "Idempotency-Key", "k-2")
	expect(code, 200, "rotate replay")
	code, _ = r.do("POST", base+"/"+sec.ID+":rotate", "admin", jsonBody(map[string]any{"value": value + "-other"}), "Idempotency-Key", "k-2")
	expect(code, 409, "rotate key reuse with another value")
	code, _ = r.do("PUT", base+"/"+sec.ID+"/value", "viewer", jsonBody(val))
	expect(code, 403, "viewer put")
	// Secrets Manager failures.
	r.w.PutErr = errors.New("secretsmanager: connection reset")
	code, _ = r.do("PUT", base+"/"+sec.ID+"/value", "admin", jsonBody(val))
	expect(code, 503, "put when Secrets Manager is down")
	code, _ = r.do("POST", base+"/"+sec.ID+":rotate", "admin", rot)
	expect(code, 503, "rotate when Secrets Manager is down")
	r.w.PutErr, r.w.CreateErr = nil, errors.New("secretsmanager: connection reset")
	code, _ = r.do("POST", base, "admin", jsonBody(map[string]any{"scope": "team", "name": "fuzz-down", "value": value}))
	expect(code, 503, "create when Secrets Manager is down")
	r.w.CreateErr = nil
	// Metadata routes.
	code, _ = r.do("PATCH", base+"/"+sec.ID, "admin", jsonBody(map[string]any{"description": "d"}), "If-Match", `"1"`)
	if code != 200 && code != 412 {
		t.Fatalf("update: %d", code)
	}
	code, _ = r.do("PATCH", base+"/"+sec.ID, "admin", jsonBody(map[string]any{"value": value}), "If-Match", `"1"`)
	expect(code, 400, "update with a value")
	code, _ = r.do("GET", base+"/"+sec.ID, "admin", "")
	expect(code, 200, "get")
	code, _ = r.do("GET", base, "viewer", "")
	expect(code, 200, "list")
	code, _ = r.do("DELETE", base+"/"+sec.ID, "admin", "", "If-Match", `"99"`)
	expect(code, 412, "delete stale")
}

// leaks returns where value appears in anything the service produced.
func (r *rig) leaks(value string) []string {
	needles := []string{marker}
	if value != "" {
		esc, _ := json.Marshal(value)
		needles = append(needles, value, strings.Trim(string(esc), `"`))
	}
	var audit, events strings.Builder
	for _, e := range r.audit.Entries {
		b, err := json.Marshal(e)
		fmt.Fprintf(&audit, "%s %+v\n", b, e)
		_ = err
	}
	for _, e := range r.outbox.Events {
		b, _ := json.Marshal(e)
		fmt.Fprintf(&events, "%s %+v\n", b, e)
	}
	var spans strings.Builder
	for _, s := range r.spans.Ended() {
		fmt.Fprintf(&spans, "%s %v %v\n", s.Name(), s.Attributes(), s.Events())
	}
	where := map[string]string{"response": r.seen.String(), "log": r.logs.buf.String(), "audit": audit.String(),
		"event": events.String(), "trace": spans.String()}
	var out []string
	for name, text := range where {
		for _, n := range needles {
			if strings.Contains(text, n) {
				out = append(out, fmt.Sprintf("%s contains %.40q", name, n))
			}
		}
	}
	return out
}

var traceOnce sync.Once

func run(t *testing.T, fuzzed string) {
	t.Helper()
	if !utf8.ValidString(fuzzed) {
		t.Skip("not valid UTF-8; the API refuses it before anything is stored")
	}
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	old := slog.Default()
	lc := &logCapture{}
	slog.SetDefault(slog.New(lc))
	defer slog.SetDefault(old)
	r := newRig(t)
	r.logs, r.spans = lc, rec
	value := marker + fuzzed
	r.exercise(t, value)
	if leaked := r.leaks(value); len(leaked) > 0 {
		t.Fatalf("the secret value leaked: %v", leaked)
	}
	if len(r.w.Written) == 0 {
		t.Fatal("the value never reached Secrets Manager: the test exercised nothing")
	}
}

// FuzzSecretValueNeverEchoed is the fuzz test of RA-US-5.3. Plain `go test` runs the seed corpus.
func FuzzSecretValueNeverEchoed(f *testing.F) {
	for _, s := range []string{
		"", "a", "p@ssw0rd!", `quote"inside`, `back\slash`, "newline\nin\tvalue", "unicode ☃ ключ 密码 🔑",
		`{"nested":"json"}`, "</script><b>html</b>", "'; DROP TABLE secrets;--", "%s %v %d %!(EXTRA", "null", "true", "123456",
		strings.Repeat("A", 4096), "-----BEGIN PRIVATE KEY-----\nMIIEvQ\n-----END PRIVATE KEY-----", "\u0000\u0001", "  ",
	} {
		f.Add(s)
	}
	f.Fuzz(run)
}
