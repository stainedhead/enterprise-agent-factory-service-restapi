package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

// Case describes how to call one operation successfully. The harness sends it as every caller and
// compares each answer with the oracle, so the request must be valid for any caller that is allowed.
type Case struct {
	// Op is the operationId.
	Op string
	// Path may use {teamId} (team A), {otherTeamId} (team B) and the keys Prepare returns.
	Path string
	// Body is the JSON request body, with the same placeholders.
	Body string
	// Headers are extra request headers (If-Match, Idempotency-Key, ...), with the same placeholders.
	Headers map[string]string
	// Prepare makes the resource the request names exist (it runs before every call, so a destructive
	// operation can be repeated) and returns placeholder values.
	Prepare func(t *testing.T, w *World) map[string]string
	// Flags are the team A flags the case runs with; conditional grants follow them.
	Flags authz.TeamFlags
	// NoOutbox exempts a mutation from the outbox check, with the reason (API contract CD-7).
	NoOutbox string
	// Suspended marks a case whose Prepare leaves team A suspended. A key of a suspended team does not
	// authenticate (database contract §4.2) and neither does a workload of it (afs.resolve_workload needs an
	// active team): both get 401 and the oracle's answer is overridden.
	Suspended bool
	// NoAudit exempts a mutation from the audit check, with the reason.
	NoAudit string
}

var (
	regMu   sync.Mutex
	cases   = map[string]Case{}
	exempts = map[string]string{}
)

// Register adds the case of an operation. Registering one operation twice panics: each operation has
// one owner stream.
func Register(c Case) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := cases[c.Op]; dup {
		panic("matrix: case for " + c.Op + " registered twice")
	}
	cases[c.Op] = c
}

// Exempt excuses an implemented operation from the matrix, with the reason. Use it only for
// operations that cannot be driven by one request (for example a streaming operation).
func Exempt(op, reason string) {
	regMu.Lock()
	defer regMu.Unlock()
	exempts[op] = reason
}

// probeTeam is a well-formed team id that exists nowhere: a service admin passes authorization for it
// and an implemented handler answers 404 or 400, never 501.
const probeTeam = "team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ"

// Implemented lists the operations whose handler is bound, found by calling each as a service admin
// and looking for the 501 not_implemented placeholder.
func Implemented(t *testing.T, w *World, h http.Handler, c *Contract) []string {
	t.Helper()
	var out []string
	for id, op := range c.Operations {
		path := strings.NewReplacer("{teamId}", probeTeam).Replace(op.Path)
		path = fillGeneric(path)
		rec := send(h, op.Method, path, w.Tokens[ServiceAdmin], "", map[string]string{"If-Match": "\"1\"", "Idempotency-Key": "mx-probe-key"}, "mx-probe")
		if rec.Code == http.StatusNotImplemented && strings.Contains(rec.Body.String(), "not_implemented") {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// fillGeneric replaces every remaining {placeholder} with a well-formed unknown id.
func fillGeneric(p string) string {
	for {
		i := strings.Index(p, "{")
		if i < 0 {
			return p
		}
		j := strings.Index(p[i:], "}")
		p = p[:i] + "zzzzzzzz" + p[i+j+1:]
	}
}

func send(h http.Handler, method, path, token, body string, hdr map[string]string, rid string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r.Header.Set("X-Request-Id", rid)
	r.RemoteAddr = "198.51.100.200:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// Run drives the matrix. It fails for an implemented operation with no case and no exemption, runs
// every case as every caller that has a credential, and checks each answer, its audit rows and its
// outbox rows.
func Run(t *testing.T, w *World, h http.Handler) {
	t.Helper()
	c, err := LoadContract()
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]httpapi.Route{}
	for _, rt := range httpapi.Routes {
		routes[rt.OperationID] = rt
	}
	regMu.Lock()
	cs, ex := map[string]Case{}, map[string]string{}
	for k, v := range cases {
		cs[k] = v
	}
	for k, v := range exempts {
		ex[k] = v
	}
	regMu.Unlock()

	implemented := Implemented(t, w, h, c)
	for _, id := range implemented {
		if _, ok := cs[id]; !ok && ex[id] == "" {
			t.Errorf("operation %s is implemented but has no matrix case: add matrix.Register in internal/adapter/httpapi/matrix/fixtures_<area>.go", id)
		}
	}
	for _, id := range implemented {
		cse, ok := cs[id]
		if !ok {
			continue
		}
		op := c.Operations[id]
		rt := routes[id]
		t.Run(id, func(t *testing.T) {
			for _, caller := range AllCallers {
				token, has := w.Tokens[caller]
				if caller != Anonymous && !has {
					continue
				}
				t.Run(string(caller), func(t *testing.T) { runOne(t, w, h, c, op, rt, cse, caller, token) })
			}
		})
	}
}

func runOne(t *testing.T, w *World, h http.Handler, c *Contract, op Operation, rt httpapi.Route, cse Case, caller Caller, token string) {
	t.Helper()
	w.SetFlags(t, cse.Flags)
	if caller == Workload {
		w.seedWorkload(t)
	}
	subst := map[string]string{"{teamId}": w.TeamA.ID, "{otherTeamId}": w.TeamB.ID}
	if cse.Prepare != nil {
		for k, v := range cse.Prepare(t, w) {
			subst["{"+k+"}"] = v
		}
	}
	apply := func(s string) string {
		for k, v := range subst {
			s = strings.ReplaceAll(s, k, v)
		}
		return s
	}
	hdr := map[string]string{}
	for k, v := range cse.Headers {
		hdr[k] = apply(v)
	}
	rid := fmt.Sprintf("mx-%s-%s-%d", op.OperationID, caller, nextSeq())
	if len(rid) > 64 {
		rid = rid[:64]
	}
	outboxBefore := w.count(t, `SELECT count(*) FROM afs.outbox_events`)
	rec := send(h, op.Method, apply(cse.Path), token, apply(cse.Body), hdr, rid)

	want := c.Expected(op, caller, cse.Flags)
	if cse.Suspended && (caller == TeamKeyViewer || caller == TeamKeyAdmin || caller == Workload) {
		want = Unauthorized
	}
	got := classify(rec.Code)
	if got != want {
		t.Fatalf("%s %s as %s: %d %s, want %s", op.Method, apply(cse.Path), caller, rec.Code, rec.Body.String(), want)
	}
	if rec.Header().Get("X-Request-Id") != rid || rec.Header().Get("X-Afs-Contract-Version") == "" {
		t.Errorf("contract headers missing: %v", rec.Header())
	}
	rows := w.auditRows(t, rid)
	switch want {
	case Forbidden:
		var env struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Code != "AccessDenied" {
			t.Fatalf("403 body: %s", rec.Body.String())
		}
		if op.Permission != "" && env.Details["requiredPermission"] != op.Permission {
			t.Errorf("requiredPermission = %v, want %s", env.Details["requiredPermission"], op.Permission)
		}
		if rows["denied"] != 1 || len(rows) != 1 {
			t.Errorf("a 403 must write exactly one denied audit record, got %v", rows)
		}
	case NotFound, Unauthorized:
		if len(rows) != 0 {
			t.Errorf("%s wrote audit records %v", want, rows)
		}
	case Allowed:
		if rows["denied"] != 0 {
			t.Errorf("an allowed request wrote a denied record: %v", rows)
		}
		if rt.Mutation && cse.NoAudit == "" && rows["allowed"] != 1 {
			t.Errorf("a mutation must write exactly one audit record, got %v (CT-API-8)", rows)
		}
		if rt.Mutation && cse.NoOutbox == "" && w.count(t, `SELECT count(*) FROM afs.outbox_events`) <= outboxBefore {
			t.Errorf("a mutation must write an outbox event (CT-API-8)")
		}
		checkETag(t, op, rec)
	}
	if want != Allowed && w.count(t, `SELECT count(*) FROM afs.outbox_events`) != outboxBefore {
		t.Errorf("a refused request wrote an outbox event")
	}
}

// checkETag verifies CT-API-13 on a successful single-resource response.
func checkETag(t *testing.T, op Operation, rec *httptest.ResponseRecorder) {
	t.Helper()
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		return
	}
	var probe struct {
		Version *int64 `json:"version"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &probe) == nil && probe.Version != nil {
		if want := fmt.Sprintf(`"%d"`, *probe.Version); rec.Header().Get("ETag") != want {
			t.Errorf("%s: ETag = %q, want %s (CT-API-13)", op.OperationID, rec.Header().Get("ETag"), want)
		}
	}
}

func classify(code int) Outcome {
	switch {
	case code/100 == 2:
		return Allowed
	case code == http.StatusUnauthorized:
		return Unauthorized
	case code == http.StatusNotFound:
		return NotFound
	case code == http.StatusForbidden:
		return Forbidden
	}
	return Outcome(-1)
}

var (
	seqMu sync.Mutex
	seq   int
)

func nextSeq() int {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return seq
}

func (w *World) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := w.Admin.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// auditRows counts the audit records of one request by outcome.
func (w *World) auditRows(t *testing.T, rid string) map[string]int {
	t.Helper()
	rows, err := w.Admin.Query(t.Context(), `SELECT outcome, count(*) FROM afs.audit_log WHERE request_id = $1 GROUP BY outcome`, rid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var o string
		var n int
		if err := rows.Scan(&o, &n); err != nil {
			t.Fatal(err)
		}
		out[o] = n
	}
	return out
}
