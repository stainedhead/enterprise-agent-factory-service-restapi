package fakes_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

var sess = port.Session{Actor: "usr_01JD3Z5Y8K2M4N6P8Q0R2S4T6V", ActorType: port.ActorUser, TeamIDs: []string{"team_a"}}

var errBoom = errors.New("boom")

func TestWritesVisibleOnlyOnCommit(t *testing.T) {
	ctx := t.Context()
	r := &fakes.TxRunner{}
	a := &fakes.Audit{}
	o := &fakes.Outbox{}
	ev := domain.Event{Type: domain.EventTeamCreated, AggregateKind: "teams", AggregateID: "team_a"}

	err := r.InTx(ctx, sess, func(ctx context.Context, tx port.Tx) error {
		if err := a.Write(ctx, tx, port.AuditRecord{Action: "CreateTeam", Outcome: port.OutcomeAllowed}); err != nil {
			return err
		}
		if err := o.Publish(ctx, tx, ev); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) || len(a.Entries) != 0 || len(o.Events) != 0 || r.Rollbacks != 1 {
		t.Fatalf("rollback: err=%v audit=%d outbox=%d rollbacks=%d", err, len(a.Entries), len(o.Events), r.Rollbacks)
	}

	err = r.InTx(ctx, sess, func(ctx context.Context, tx port.Tx) error {
		if tx.Session().Actor != sess.Actor {
			t.Errorf("session not passed")
		}
		if err := a.Write(ctx, tx, port.AuditRecord{Action: "CreateTeam", Outcome: port.OutcomeAllowed}); err != nil {
			return err
		}
		return o.Publish(ctx, tx, ev)
	})
	if err != nil || len(a.Entries) != 1 || len(o.Events) != 1 || r.Commits != 1 || len(r.Sessions) != 2 {
		t.Fatalf("commit: err=%v audit=%d outbox=%d commits=%d", err, len(a.Entries), len(o.Events), r.Commits)
	}

	if err := a.WriteDenied(ctx, sess, port.AuditRecord{Action: "DeleteTeam"}); err != nil {
		t.Fatal(err)
	}
	if e := a.Entries[1]; !e.Own || e.Record.Outcome != port.OutcomeDenied {
		t.Fatalf("denied entry %+v", e)
	}
}

func TestReadTxRefusesWrites(t *testing.T) {
	r := &fakes.TxRunner{}
	err := r.ReadTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		if !tx.(*fakes.Tx).ReadOnly() {
			t.Error("not read-only")
		}
		return (&fakes.Audit{}).Write(ctx, tx, port.AuditRecord{})
	})
	if !errors.Is(err, fakes.ErrReadOnly) {
		t.Fatalf("got %v", err)
	}
}

func TestRunnerErr(t *testing.T) {
	r := &fakes.TxRunner{Err: errBoom}
	ran := false
	err := r.InTx(t.Context(), sess, func(context.Context, port.Tx) error { ran = true; return nil })
	if !errors.Is(err, errBoom) || ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
}

type otherTx struct{}

func (otherTx) Session() port.Session { return port.Session{} }

func TestForeignTx(t *testing.T) {
	if err := (&fakes.Audit{}).Write(t.Context(), otherTx{}, port.AuditRecord{}); err == nil {
		t.Fatal("want error for a foreign transaction")
	}
}

func TestOutboxRejectsUnknownType(t *testing.T) {
	r := &fakes.TxRunner{}
	err := r.InTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		return (&fakes.Outbox{}).Publish(ctx, tx, domain.Event{Type: "nope.nope"})
	})
	if err == nil {
		t.Fatal("want error")
	}
}

func TestIdempotency(t *testing.T) {
	ctx := t.Context()
	r := &fakes.TxRunner{}
	s := &fakes.Idempotency{}
	k := port.IdempotencyKey{Key: "k1", Operation: "CreateApiKey", RequestSHA256: sha256.Sum256([]byte("a"))}
	body := json.RawMessage(`{"id":"key_1","secretKey":"afsk_x","nested":[{"secretKey":"y","n":1}]}`)

	// A 5xx rolls back and the key disappears.
	_ = r.InTx(ctx, sess, func(ctx context.Context, tx port.Tx) error {
		if got, err := s.Begin(ctx, tx, k); got != nil || err != nil {
			t.Fatalf("first begin: %v %v", got, err)
		}
		if err := s.Complete(ctx, tx, k, port.StoredResponse{Status: 503}); err == nil {
			t.Error("5xx stored")
		}
		return errBoom
	})
	if err := r.InTx(ctx, sess, func(ctx context.Context, tx port.Tx) error {
		if got, err := s.Begin(ctx, tx, k); got != nil || err != nil {
			return errors.Join(errBoom, err)
		}
		return s.Complete(ctx, tx, k, port.StoredResponse{Status: 201, Body: body})
	}); err != nil {
		t.Fatal(err)
	}

	_ = r.InTx(ctx, sess, func(ctx context.Context, tx port.Tx) error {
		got, err := s.Begin(ctx, tx, k)
		if err != nil || got == nil || got.Status != 201 {
			t.Fatalf("replay: %v %v", got, err)
		}
		if string(got.Body) != `{"id":"key_1","nested":[{"n":1}]}` {
			t.Fatalf("stored body %s", got.Body)
		}
		k2 := k
		k2.RequestSHA256 = sha256.Sum256([]byte("b"))
		if _, err := s.Begin(ctx, tx, k2); domain.KindOf(err) != domain.KindConflict {
			t.Fatalf("mismatch: %v", err)
		}
		k3 := k
		k3.Key = "k3"
		if _, err := s.Begin(ctx, tx, k3); err != nil {
			t.Fatal(err)
		}
		_, err = s.Begin(ctx, tx, k3)
		var de *domain.Error
		if !errors.As(err, &de) || de.Reason != domain.ReasonIdempotencyInProgress || de.RetryAfterSeconds == 0 {
			t.Fatalf("in progress: %v", err)
		}
		if err := s.Complete(ctx, tx, port.IdempotencyKey{Key: "none"}, port.StoredResponse{Status: 200}); err == nil {
			t.Error("Complete without Begin")
		}
		if err := s.Complete(ctx, tx, k3, port.StoredResponse{Status: 200, Body: json.RawMessage(`{`)}); err == nil {
			t.Error("bad JSON stored")
		}
		return nil
	})
}

func TestStripSecretKeyEmpty(t *testing.T) {
	got, err := port.StripSecretKey(nil)
	if err != nil || got != nil {
		t.Fatalf("%s %v", got, err)
	}
}

func TestClockAndIDs(t *testing.T) {
	c := &fakes.Clock{T: time.Unix(100, 0)}
	c.Advance(time.Second)
	if c.Now().Unix() != 101 {
		t.Fatal(c.Now())
	}
	g := &fakes.IDGen{}
	a, _ := g.NewID(domain.PrefixTeam)
	b, err := g.NewID(domain.PrefixTeam)
	if err != nil || a == b {
		t.Fatalf("%s %s %v", a, b, err)
	}
	if _, err := domain.ParseID(domain.PrefixTeam, string(b)); err != nil {
		t.Fatal(err)
	}
}

func TestSettings(t *testing.T) {
	s := &fakes.Settings{}
	v, err := s.Setting(t.Context(), "api.idempotency_key_ttl_seconds")
	if err != nil || string(v) != "3600" {
		t.Fatalf("%s %v", v, err)
	}
	s.Set("api.idempotency_key_ttl_seconds", json.RawMessage("60"))
	if v, _ := s.Setting(t.Context(), "api.idempotency_key_ttl_seconds"); string(v) != "60" {
		t.Fatalf("%s", v)
	}
	if _, err := s.Setting(t.Context(), "no.such"); domain.KindOf(err) != domain.KindValidation {
		t.Fatal(err)
	}
}

func TestCredentials(t *testing.T) {
	sum := sha256.Sum256([]byte("afsk_x"))
	c := &fakes.Credentials{
		Keys:      map[[32]byte]port.APIKeyRecord{sum: {KeyID: "key_1", State: "active"}},
		Workloads: map[[2]string]port.WorkloadRecord{{"afs-a", "agent-x"}: {TeamID: "team_a", AgentID: "agt_1"}},
		Teams:     []port.TeamDirectoryEntry{{ID: "team_a", Slug: "a"}},
	}
	if r, ok, _ := c.AuthenticateAPIKey(t.Context(), sum); !ok || r.KeyID != "key_1" {
		t.Fatal(r)
	}
	if _, ok, _ := c.ResolveWorkload(t.Context(), "afs-a", "agent-y"); ok {
		t.Fatal("unknown workload resolved")
	}
	if _, err := c.ListTeamDirectory(t.Context(), sess); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatal(err)
	}
	if got, err := c.ListTeamDirectory(t.Context(), port.Session{SecurityOperator: true}); err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
}

func TestObjectStoreFake(t *testing.T) {
	ctx := context.Background()
	clock := &fakes.Clock{T: time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC)}
	s := &fakes.ObjectStore{Clock: clock}
	if _, err := s.PresignGet(ctx, "missing", time.Hour); err == nil {
		t.Error("presigned a missing object")
	}
	res, err := s.Put(ctx, port.PutObject{Key: "reports/a.csv", Body: []byte("x,y\n"), ContentType: "text/csv"})
	if err != nil || res.URI != "s3://example-reports/reports/a.csv" || res.Size != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := s.Put(ctx, port.PutObject{Key: "other/b.csv", Body: []byte("z")}); err != nil {
		t.Fatal(err)
	}
	if body, err := s.Body("reports/a.csv"); err != nil || string(body) != "x,y\n" {
		t.Fatalf("body %q %v", body, err)
	}
	if _, err := s.Body("nope"); !errors.Is(err, fakes.ErrNoObject) {
		t.Errorf("body of a missing key: %v", err)
	}
	u, err := s.PresignGet(ctx, "reports/a.csv", 7*24*time.Hour)
	if err != nil || !u.ExpiresAt.Equal(clock.T.Add(7*24*time.Hour)) || !strings.Contains(u.URL, "X-Amz-Expires=604800") {
		t.Fatalf("%+v %v", u, err)
	}
	got, err := s.List(ctx, "reports/")
	if err != nil || len(got) != 1 || got[0].Key != "reports/a.csv" {
		t.Fatalf("%+v %v", got, err)
	}
	boom := errors.New("boom")
	s.PutErr, s.PresignErr, s.ListErr = boom, boom, boom
	if _, err := s.Put(ctx, port.PutObject{Key: "k"}); !errors.Is(err, boom) {
		t.Error(err)
	}
	if _, err := s.PresignGet(ctx, "reports/a.csv", time.Hour); !errors.Is(err, boom) {
		t.Error(err)
	}
	if _, err := s.List(ctx, ""); !errors.Is(err, boom) {
		t.Error(err)
	}
	if s.Puts != 2 {
		t.Errorf("puts = %d", s.Puts)
	}
}
