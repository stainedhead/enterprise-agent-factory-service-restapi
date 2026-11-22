package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

const user = "usr_01JD3Z5Y8K2M4N6P8Q0R2S4T6V"

func appPool(t *testing.T, db *pgtest.DB) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_app"], Service: "afs-api", MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func userSession(teams ...string) port.Session {
	return port.Session{Actor: user, ActorType: port.ActorUser, TeamIDs: teams, RequestID: "req_test"}
}

func countTeams(t *testing.T, tx port.Tx) int {
	t.Helper()
	pt, err := postgres.Unwrap(tx)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pt.QueryRow(t.Context(), `SELECT count(*) FROM afs.teams`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// M0-F2.
func TestMigrateAndSchema(t *testing.T) {
	db := pgtest.Get(t)
	want, err := postgres.ExpectedSchemaVersion()
	if err != nil || want != 18 {
		t.Fatalf("expected version 18, got %d, %v", want, err)
	}
	// Three concurrent starts under the advisory lock: all succeed, none applies anything twice.
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = postgres.Migrate(t.Context(), db.URLs["afs_owner"], 5*time.Second, nil)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	owner, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_owner"], Service: "afs-migrate"})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if have, _, err := postgres.CheckSchema(t.Context(), owner); err != nil || have != want {
		t.Fatalf("owner CheckSchema: have %d err %v", have, err)
	}
	// An older schema is refused.
	tx, err := db.Admin(t).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `DELETE FROM public.afs_schema_migrations WHERE version = $1`, want); err != nil {
		t.Fatal(err)
	}
	if have, _, err := postgres.CheckSchema(t.Context(), tx); !errors.Is(err, postgres.ErrSchemaTooOld) || have != want-1 {
		t.Fatalf("older schema: have %d err %v", have, err)
	}
}

func TestServiceRoleSystemActor(t *testing.T) {
	db := pgtest.Get(t)
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_reconciler"], Service: "afs-reconciler", SystemActor: true, StatementTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var actor, timeout string
	if err := pool.QueryRow(t.Context(), `SELECT afs.current_actor(), current_setting('statement_timeout')`).Scan(&actor, &timeout); err != nil {
		t.Fatal(err)
	}
	if actor != "system:afs-reconciler" || timeout != "5s" {
		t.Fatalf("actor %q timeout %q", actor, timeout)
	}
	// X-DB-7: every service role can run the startup version check through afs.schema_version().
	have, want, err := postgres.CheckSchema(t.Context(), pool)
	if err != nil || have != want {
		t.Fatalf("service role schema check: have %d want %d err %v", have, want, err)
	}
}

func TestOpenPoolPasswordFunc(t *testing.T) {
	db := pgtest.Get(t)
	called := false
	_, err := postgres.OpenPool(t.Context(), postgres.Config{
		URL: db.URLs["afs_app"],
		Password: func(context.Context) (string, error) {
			called = true
			return "", errors.New("no token")
		},
	})
	if err == nil || !called {
		t.Fatalf("err %v called %v", err, called)
	}
	if _, err := postgres.OpenPool(t.Context(), postgres.Config{URL: "::bad::"}); err == nil {
		t.Fatal("bad URL accepted")
	}
	if _, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_app"], SystemActor: true}); err == nil {
		t.Fatal("SystemActor without Service accepted")
	}
}

// M0-F3.
func TestTxRunnerSession(t *testing.T) {
	db := pgtest.Get(t)
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	r := postgres.NewTxRunner(appPool(t, db))
	ctx := t.Context()

	// A session bound to no team sees no team rows (fail closed).
	if err := r.ReadTx(ctx, userSession(), func(ctx context.Context, tx port.Tx) error {
		if n := countTeams(t, tx); n != 0 {
			t.Errorf("unbound session sees %d teams", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Bound to a: sees a only; settings are SET LOCAL.
	if err := r.ReadTx(ctx, userSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		pt, _ := postgres.Unwrap(tx)
		var id, actor, iso, ro string
		if err := pt.QueryRow(ctx, `SELECT (SELECT string_agg(id, ',') FROM afs.teams), afs.current_actor(),
			current_setting('transaction_isolation'), current_setting('transaction_read_only')`).Scan(&id, &actor, &iso, &ro); err != nil {
			return err
		}
		if id != a.ID || actor != user || iso != "read committed" || ro != "on" {
			t.Errorf("id %s actor %s iso %s ro %s", id, actor, iso, ro)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Service admin sees both; security operator alone sees none.
	_ = r.ReadTx(ctx, port.Session{Actor: user, ActorType: port.ActorUser, ServiceAdmin: true}, func(ctx context.Context, tx port.Tx) error {
		if n := countTeams(t, tx); n < 2 {
			t.Errorf("service admin sees %d", n)
		}
		return nil
	})
	_ = r.ReadTx(ctx, port.Session{Actor: user, ActorType: port.ActorUser, SecurityOperator: true}, func(ctx context.Context, tx port.Tx) error {
		if n := countTeams(t, tx); n != 0 {
			t.Errorf("security operator sees %d teams", n)
		}
		return nil
	})
	// Settings do not leak into the next transaction on the same connection.
	pool := appPool(t, db)
	r1 := postgres.NewTxRunner(pool)
	_ = r1.ReadTx(ctx, userSession(b.ID), func(context.Context, port.Tx) error { return nil })
	var leaked string
	if err := pool.QueryRow(ctx, `SELECT coalesce(current_setting('afs.team_ids', true), '')`).Scan(&leaked); err != nil || leaked != "" {
		t.Fatalf("leaked %q %v", leaked, err)
	}
	// A read-only transaction refuses writes.
	err := r.ReadTx(ctx, userSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		pt, _ := postgres.Unwrap(tx)
		_, err := pt.Exec(ctx, `UPDATE afs.teams SET display_name = 'x' WHERE id = $1`, a.ID)
		return err
	})
	if err == nil {
		t.Fatal("write in read-only transaction")
	}
	// Invalid sessions are refused before touching the database.
	for _, s := range []port.Session{{}, {Actor: user, TeamIDs: []string{"a,b"}}} {
		if err := r.InTx(ctx, s, func(context.Context, port.Tx) error { return nil }); err == nil {
			t.Errorf("session %+v accepted", s)
		}
	}
	if _, err := postgres.Unwrap(nil); err == nil {
		t.Fatal("Unwrap(nil)")
	}
}

func TestTxRunnerRetriesSerializationFailures(t *testing.T) {
	db := pgtest.Get(t)
	r := postgres.NewTxRunner(appPool(t, db))
	ctx := t.Context()
	for _, tc := range []struct {
		code     string
		failures int
		attempts int
		ok       bool
	}{
		{"40001", 2, 3, true},
		{"40P01", 1, 2, true},
		{"40001", 10, 4, false}, // first try + 3 retries, then InternalError
		{"23505", 10, 1, false}, // not retried
	} {
		attempts := 0
		err := r.InTx(ctx, userSession(), func(context.Context, port.Tx) error {
			attempts++
			if attempts <= tc.failures {
				return &pgconn.PgError{Code: tc.code}
			}
			return nil
		})
		if attempts != tc.attempts || (err == nil) != tc.ok {
			t.Errorf("%s x%d: attempts %d err %v", tc.code, tc.failures, attempts, err)
		}
		if !tc.ok && tc.code == "40001" && domain.KindOf(err) != domain.KindInternal {
			t.Errorf("exhausted retries: %v", err)
		}
	}
	// A real serialization failure: two transactions upgrade to SERIALIZABLE is not possible under
	// READ COMMITTED, so a deadlock is provoked instead.
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	s := port.Session{Actor: user, ActorType: port.ActorUser, ServiceAdmin: true}
	var wg sync.WaitGroup
	step := make(chan struct{})
	var once sync.Once
	attempts := [2]int{}
	errs := [2]error{}
	for i, order := range [][2]string{{a.ID, b.ID}, {b.ID, a.ID}} {
		wg.Go(func() {
			errs[i] = r.InTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
				attempts[i]++
				pt, _ := postgres.Unwrap(tx)
				if _, err := pt.Exec(ctx, `UPDATE afs.teams SET display_name = display_name WHERE id = $1`, order[0]); err != nil {
					return err
				}
				if attempts[i] == 1 {
					once.Do(func() { close(step) })
					<-step
					time.Sleep(100 * time.Millisecond)
				}
				_, err := pt.Exec(ctx, `UPDATE afs.teams SET display_name = display_name WHERE id = $1`, order[1])
				return err
			})
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || attempts[0]+attempts[1] < 3 {
		t.Fatalf("deadlock retry: errs %v attempts %v", errs, attempts)
	}
}

func TestTxRunnerDatabaseDown(t *testing.T) {
	db := pgtest.Get(t)
	pool := appPool(t, db)
	r := postgres.NewTxRunner(pool)
	pool.Close()
	err := r.InTx(t.Context(), userSession(), func(context.Context, port.Tx) error { return nil })
	if domain.KindOf(err) != domain.KindUnavailable && domain.KindOf(err) != domain.KindInternal {
		t.Fatalf("closed pool: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	err = postgres.NewTxRunner(appPool(t, db)).InTx(ctx, userSession(), func(context.Context, port.Tx) error { return nil })
	if domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("deadline: %v", err)
	}
}

// M0-F4 against the real schema.
func TestSQLStateMapping(t *testing.T) {
	db := pgtest.Get(t)
	a := db.SeedTeam(t)
	r := postgres.NewTxRunner(appPool(t, db))
	ctx := t.Context()
	exec := func(s port.Session, op postgres.Op, sql string, args ...any) error {
		return r.InTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
			pt, _ := postgres.Unwrap(tx)
			_, err := pt.Exec(ctx, sql, args...)
			return postgres.MapError(err, op, "team")
		})
	}
	admin := port.Session{Actor: user, ActorType: port.ActorUser, ServiceAdmin: true}
	tests := []struct {
		name string
		s    port.Session
		op   postgres.Op
		sql  string
		args []any
		want domain.ErrorKind
	}{
		{"duplicate slug", admin, postgres.OpWrite, `INSERT INTO afs.teams (id, slug, display_name, cost_center, owner_email, max_budget_usd) VALUES ($1, $2, 'x', 'CC', 'o@example.com', 1)`, []any{pgtest.NewID("team"), a.Slug}, domain.KindConflict},
		{"bad id", admin, postgres.OpWrite, `INSERT INTO afs.teams (id, slug, display_name, cost_center, owner_email, max_budget_usd) VALUES ('nope', 'nope', 'x', 'CC', 'o@example.com', 1)`, nil, domain.KindValidation},
		{"not null", admin, postgres.OpWrite, `INSERT INTO afs.teams (id, slug) VALUES ($1, 'nn')`, []any{pgtest.NewID("team")}, domain.KindValidation},
		{"unknown reference", admin, postgres.OpWrite, `INSERT INTO afs.budgets (id, team_id, amount_usd) VALUES ($1, $2, 1)`, []any{pgtest.NewID("bud"), pgtest.NewID("team")}, domain.KindValidation},
		{"audit is append-only", admin, postgres.OpWrite, `UPDATE afs.audit_log SET action = 'x'`, nil, domain.KindAccessDenied},
		{"bad cast", admin, postgres.OpRead, `SELECT 'x'::int`, nil, domain.KindValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := exec(tt.s, tt.op, tt.sql, tt.args...); domain.KindOf(err) != tt.want {
				t.Fatalf("got %v, want %s", err, tt.want)
			}
		})
	}
}

// M0-F5.
func TestAuditAndOutboxInOneTransaction(t *testing.T) {
	db := pgtest.Get(t)
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	r := postgres.NewTxRunner(appPool(t, db))
	audit := postgres.NewAuditWriter(r)
	outbox := postgres.NewOutboxWriter()
	ctx := t.Context()
	admin := db.Admin(t)
	count := func(sql string, args ...any) int {
		var n int
		if err := admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ev := func(team string) domain.Event {
		return domain.Event{Type: domain.EventTeamUpdated, AggregateKind: "teams", AggregateID: team,
			Detail: domain.EventDetail{ID: pgtest.NewID("evt"), OccurredAt: time.Now(), TeamID: team, Data: map[string]any{"field": "displayName"}}}
	}
	mutate := func(fail bool, name string) error {
		return r.InTx(ctx, userSession(a.ID), func(ctx context.Context, tx port.Tx) error {
			pt, _ := postgres.Unwrap(tx)
			if _, err := pt.Exec(ctx, `UPDATE afs.teams SET display_name = $2 WHERE id = $1`, a.ID, name); err != nil {
				return err
			}
			if err := audit.Write(ctx, tx, port.AuditRecord{Action: "UpdateTeam", Outcome: port.OutcomeAllowed,
				ResourceKind: "teams", ResourceID: a.ID, TeamID: a.ID, SourceIP: "192.0.2.10", UserAgent: "afs-cli",
				Before: map[string]any{"displayName": "old"}, After: map[string]any{"displayName": name, "token": "t0ps3cret"}}); err != nil {
				return err
			}
			if err := outbox.Publish(ctx, tx, ev(a.ID)); err != nil {
				return err
			}
			if fail {
				return errors.New("fail after writes")
			}
			return nil
		})
	}
	if err := mutate(true, "rolled-back"); err == nil {
		t.Fatal("want error")
	}
	if n := count(`SELECT count(*) FROM afs.teams WHERE display_name = 'rolled-back'`) +
		count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1`, a.ID) +
		count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1`, a.ID); n != 0 {
		t.Fatalf("rollback left %d rows", n)
	}
	if err := mutate(false, "committed"); err != nil {
		t.Fatal(err)
	}
	var after, actor, reqID, ip, payload string
	if err := admin.QueryRow(ctx, `SELECT after::text, actor_id, request_id, host(source_ip) FROM afs.audit_log WHERE resource_id = $1`, a.ID).
		Scan(&after, &actor, &reqID, &ip); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after, "t0ps3cret") || actor != user || reqID != "req_test" || ip != "192.0.2.10" {
		t.Fatalf("audit row after=%s actor=%s req=%s ip=%s", after, actor, reqID, ip)
	}
	if err := admin.QueryRow(ctx, `SELECT payload::text FROM afs.outbox_events WHERE aggregate_id = $1`, a.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var detail domain.EventDetail
	if err := json.Unmarshal([]byte(payload), &detail); err != nil || detail.Actor != user || detail.TeamID != a.ID {
		t.Fatalf("payload %s %v", payload, err)
	}

	// Denied: in its own transaction, even for a team the caller does not see (policy app_own_audit).
	if err := audit.WriteDenied(ctx, userSession(a.ID), port.AuditRecord{Action: "DeleteTeam", ResourceKind: "teams", ResourceID: b.ID, TeamID: b.ID}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND outcome = 'denied'`, b.ID); n != 1 {
		t.Fatalf("denied rows %d", n)
	}
	// Service route denial: no team at all.
	if err := audit.WriteDenied(ctx, userSession(), port.AuditRecord{Action: "PutSetting", ResourceKind: "service_settings"}); err != nil {
		t.Fatal(err)
	}
	// Another team's outbox event is refused for a team caller.
	err := r.InTx(ctx, userSession(a.ID), func(ctx context.Context, tx port.Tx) error { return outbox.Publish(ctx, tx, ev(b.ID)) })
	if domain.KindOf(err) != domain.KindAccessDenied && domain.KindOf(err) != domain.KindInternal {
		t.Fatalf("cross-team outbox: %v", err)
	}
	if err == nil {
		t.Fatal("cross-team outbox accepted")
	}
	// A security operator may write work order events for any team (policy app_operator_work_order_events).
	op := port.Session{Actor: user, ActorType: port.ActorUser, SecurityOperator: true}
	wo := domain.Event{Type: domain.EventWorkOrderCreated, AggregateKind: "work_orders", AggregateID: pgtest.NewID("wo"),
		Detail: domain.EventDetail{ID: pgtest.NewID("evt"), OccurredAt: time.Now(), TeamID: b.ID}}
	if err := r.InTx(ctx, op, func(ctx context.Context, tx port.Tx) error { return outbox.Publish(ctx, tx, wo) }); err != nil {
		t.Fatalf("operator work order event: %v", err)
	}
	// Writer input checks.
	for _, bad := range []domain.Event{{Type: "nope"}, {Type: domain.EventTeamUpdated}} {
		if err := r.InTx(ctx, userSession(a.ID), func(ctx context.Context, tx port.Tx) error { return outbox.Publish(ctx, tx, bad) }); err == nil {
			t.Errorf("event %+v accepted", bad)
		}
	}
	for _, bad := range []port.AuditRecord{{}, {Action: "X", Outcome: port.OutcomeAllowed, SourceIP: "nope"}, {Action: "X", Outcome: port.OutcomeAllowed, After: func() {}}} {
		if err := r.InTx(ctx, userSession(a.ID), func(ctx context.Context, tx port.Tx) error { return audit.Write(ctx, tx, bad) }); err == nil {
			t.Errorf("audit %+v accepted", bad)
		}
	}
	if err := r.InTx(ctx, port.Session{Actor: user}, func(ctx context.Context, tx port.Tx) error {
		return audit.Write(ctx, tx, port.AuditRecord{Action: "X", Outcome: port.OutcomeAllowed})
	}); err == nil {
		t.Error("audit without actor type accepted")
	}
}

func TestAuditRedactsSecretValues(t *testing.T) {
	db := pgtest.Get(t)
	a := db.SeedTeam(t)
	r := postgres.NewTxRunner(appPool(t, db))
	id := pgtest.NewID("sec")
	err := r.InTx(t.Context(), userSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		return postgres.NewAuditWriter(r).Write(ctx, tx, port.AuditRecord{Action: "CreateSecret", Outcome: port.OutcomeAllowed,
			ResourceKind: "secrets", ResourceID: id, TeamID: a.ID,
			After: map[string]any{"name": "db", "value": "hunter2", "nested": []any{map[string]any{"Password": "p", "keep": 1}}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var after string
	if err := db.Admin(t).QueryRow(t.Context(), `SELECT after::text FROM afs.audit_log WHERE resource_id = $1`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after, "hunter2") || strings.Contains(after, `"p"`) || !strings.Contains(after, `"keep"`) {
		t.Fatalf("after = %s", after)
	}
}

// M0-F6.
func TestIdempotencyStore(t *testing.T) {
	db := pgtest.Get(t)
	a := db.SeedTeam(t)
	r := postgres.NewTxRunner(appPool(t, db))
	store := postgres.NewIdempotencyStore()
	ctx := t.Context()
	s := userSession(a.ID)
	k := port.IdempotencyKey{Key: "k-" + pgtest.NewID("x"), Operation: "CreateApiKey", TeamID: a.ID, RequestSHA256: sha256.Sum256([]byte("req"))}
	body := json.RawMessage(`{"id":"key_1","secretKey":"afsk_s3cr3t","name":"ci"}`)

	// A 5xx rolls back: the key disappears and a retry runs again.
	err := r.InTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
		if got, err := store.Begin(ctx, tx, k); got != nil || err != nil {
			t.Fatalf("begin: %v %v", got, err)
		}
		if err := store.Complete(ctx, tx, k, port.StoredResponse{Status: 500}); err == nil {
			t.Error("5xx stored")
		}
		return errors.New("internal")
	})
	if err == nil {
		t.Fatal("want error")
	}
	run := func(k port.IdempotencyKey) (*port.StoredResponse, error) {
		var replay *port.StoredResponse
		err := r.InTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
			got, err := store.Begin(ctx, tx, k)
			if err != nil || got != nil {
				replay = got
				return err
			}
			return store.Complete(ctx, tx, k, port.StoredResponse{Status: 201, Body: body})
		})
		return replay, err
	}
	if got, err := run(k); got != nil || err != nil {
		t.Fatalf("first run: %v %v", got, err)
	}
	got, err := run(k)
	if err != nil || got == nil || got.Status != 201 || strings.Contains(string(got.Body), "afsk_") || !strings.Contains(string(got.Body), `"ci"`) {
		t.Fatalf("replay: %+v %v", got, err)
	}
	k2 := k
	k2.RequestSHA256 = sha256.Sum256([]byte("other"))
	if _, err := run(k2); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("mismatch: %v", err)
	}
	k3 := k
	k3.Operation = "CreateTeam"
	var de *domain.Error
	if _, err := run(k3); !errors.As(err, &de) || de.Reason != domain.ReasonIdempotencyMismatch {
		t.Fatalf("operation mismatch: %v", err)
	}
	// Keys are per principal: another caller with the same key runs.
	other := s
	other.Actor = "usr_01JD3Z5Y8K2M4N6P8Q0R2S4T7X"
	if err := r.InTx(ctx, other, func(ctx context.Context, tx port.Tx) error {
		got, err := store.Begin(ctx, tx, k)
		if got != nil {
			t.Error("other principal replayed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// In progress within one transaction.
	k4 := k
	k4.Key = "k4-" + pgtest.NewID("x")
	_ = r.InTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
		if _, err := store.Begin(ctx, tx, k4); err != nil {
			t.Fatal(err)
		}
		_, err := store.Begin(ctx, tx, k4)
		if !errors.As(err, &de) || de.Reason != domain.ReasonIdempotencyInProgress || de.RetryAfterSeconds == 0 {
			t.Fatalf("in progress: %v", err)
		}
		if err := store.Complete(ctx, tx, port.IdempotencyKey{Key: "never-begun"}, port.StoredResponse{Status: 200}); err == nil {
			t.Error("Complete without Begin")
		}
		if err := store.Complete(ctx, tx, k4, port.StoredResponse{Status: 200, Body: json.RawMessage(`{`)}); err == nil {
			t.Error("bad JSON stored")
		}
		return nil
	})
	// Expired keys are treated as absent.
	k5 := k
	k5.Key = "k5-" + pgtest.NewID("x")
	if _, err := run(k5); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Admin(t).Exec(ctx, `UPDATE afs.idempotency_keys SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE key = $1`, k5.Key); err != nil {
		t.Fatal(err)
	}
	k5.RequestSHA256 = sha256.Sum256([]byte("new"))
	if got, err := run(k5); got != nil || err != nil {
		t.Fatalf("expired key: %v %v", got, err)
	}
	// Concurrent duplicates: exactly one runs, the other replays after it commits.
	k6 := k
	k6.Key = "k6-" + pgtest.NewID("x")
	var wg sync.WaitGroup
	replays := make([]*port.StoredResponse, 4)
	errs := make([]error, 4)
	for i := range replays {
		wg.Go(func() { replays[i], errs[i] = run(k6) })
	}
	wg.Wait()
	ran := 0
	for i := range replays {
		if errs[i] != nil {
			t.Fatalf("concurrent %d: %v", i, errs[i])
		}
		if replays[i] == nil {
			ran++
		}
	}
	if ran != 1 {
		t.Fatalf("%d concurrent requests ran", ran)
	}
	if _, err := store.Begin(ctx, fakeTx{}, k); err == nil {
		t.Fatal("foreign tx accepted")
	}
}

type fakeTx struct{}

func (fakeTx) Session() port.Session { return port.Session{} }

// M0-F8 replaced by X-DB-1..3.
func TestCredentialLookups(t *testing.T) {
	db := pgtest.Get(t)
	a := db.SeedTeam(t)
	admin := db.Admin(t)
	ctx := t.Context()
	sum := sha256.Sum256([]byte("afsk_test_" + a.ID))
	svcSum := sha256.Sum256([]byte("afsk_svc_" + a.ID))
	keyID, svcID := pgtest.NewID("key"), pgtest.NewID("key")
	if _, err := admin.Exec(ctx, `INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES
		($1, $2, 'ci', 'team_admin', 'afsk_aa', $3, now() + interval '30 days'),
		($4, NULL, 'scanner', 'security_operator', 'afsk_sv', $5, now() + interval '30 days')`,
		keyID, a.ID, sum[:], svcID, svcSum[:]); err != nil {
		t.Fatal(err)
	}
	c := postgres.NewCredentials(postgres.NewTxRunner(appPool(t, db)))
	rec, ok, err := c.AuthenticateAPIKey(ctx, sum)
	if err != nil || !ok || rec.KeyID != keyID || rec.TeamID != a.ID || rec.Role != "team_admin" || !rec.Usable() || rec.Principal != keyID {
		t.Fatalf("team key: %+v %v %v", rec, ok, err)
	}
	var used *time.Time
	if err := admin.QueryRow(ctx, `SELECT last_used_at FROM afs.api_keys WHERE id = $1`, keyID).Scan(&used); err != nil || used == nil {
		t.Fatalf("last_used_at not set: %v", err)
	}
	rec, ok, err = c.AuthenticateAPIKey(ctx, svcSum)
	if err != nil || !ok || rec.TeamID != "" || rec.TeamState != "" || !rec.Usable() {
		t.Fatalf("service key: %+v %v %v", rec, ok, err)
	}
	if _, ok, err := c.AuthenticateAPIKey(ctx, sha256.Sum256([]byte("unknown"))); ok || err != nil {
		t.Fatalf("unknown key: %v %v", ok, err)
	}
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET state = 'suspended' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if rec, ok, _ := c.AuthenticateAPIKey(ctx, sum); !ok || rec.Usable() || rec.TeamState != "suspended" {
		t.Fatalf("suspended team key usable: %+v", rec)
	}
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET state = 'active' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}

	var ns string
	if err := admin.QueryRow(ctx, `SELECT namespace FROM afs.teams WHERE id = $1`, a.ID).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.ResolveWorkload(ctx, ns, "agent-nope"); ok || err != nil {
		t.Fatalf("unknown workload: %v %v", ok, err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO afs.models (id, provider, display_name) VALUES ('claude-sonnet', 'Anthropic', 'Claude Sonnet') ON CONFLICT DO NOTHING;`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'claude-sonnet')`, a.ID); err != nil {
		t.Fatal(err)
	}
	agentID := pgtest.NewID("agt")
	if _, err := admin.Exec(ctx, `INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, 'indexer', 'omp', 'ghcr.io/x/omp@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'claude-sonnet')`, agentID, a.ID); err != nil {
		t.Fatal(err)
	}
	w, ok, err := c.ResolveWorkload(ctx, ns, "agent-indexer")
	if err != nil || !ok || w.TeamID != a.ID || w.AgentID != agentID || w.PoolID != "" {
		t.Fatalf("workload: %+v %v %v", w, ok, err)
	}
	if _, ok, _ := c.ResolveWorkload(ctx, "kube-system", "agent-indexer"); ok {
		t.Fatal("other namespace resolved")
	}

	if _, err := c.ListTeamDirectory(ctx, userSession(a.ID)); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team caller: %v", err)
	} else {
		var de *domain.Error
		_ = errors.As(err, &de)
		if de.RequiredPermission == "" {
			t.Fatal("no required permission")
		}
	}
	dir, err := c.ListTeamDirectory(ctx, port.Session{Actor: user, ActorType: port.ActorUser, SecurityOperator: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range dir {
		found = found || (e.ID == a.ID && e.Slug == a.Slug)
	}
	if !found {
		t.Fatalf("team %s not in directory %+v", a.ID, dir)
	}
}
