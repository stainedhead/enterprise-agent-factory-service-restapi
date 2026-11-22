package postgres_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// M3-G1, M3-G2: channel rows round trip for a team and for the service, with optimistic concurrency,
// unique names, team isolation, the team-secret rule and the work order setting lookup.
func TestChannelRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewChannelRepo()
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	secA := teamSecret(t, db, a.ID, "slack")
	secB := teamSecret(t, db, b.ID, "slack")
	now := time.Now()
	slack := domain.NewChannelParams{Name: "slack", Kind: domain.ChannelSlack, Enabled: true, SecretID: secA,
		Config: map[string]any{"channel": "#alerts"}, EventTypes: []string{"budget.*"}}
	mail := domain.NewChannelParams{Name: "mail", Kind: domain.ChannelEmail, Enabled: true, Config: map[string]any{"to": []any{"ops@example.com"}}}
	slackC, _ := slack.New(pgtest.NewID("nch"), a.ID, now)
	mailC, _ := mail.New(pgtest.NewID("nch"), a.ID, now)
	foreign, _ := mail.New(pgtest.NewID("nch"), b.ID, now)
	svc, _ := mail.New(pgtest.NewID("nch"), "", now)
	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		for _, c := range []domain.NotificationChannel{slackC, mailC, foreign, svc} {
			if err := repo.Insert(ctx, tx, c); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dup, _ := mail.New(pgtest.NewID("nch"), a.ID, now)
	_ = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.Insert(ctx, tx, dup), domain.KindConflict)
		return context.Canceled
	})
	cross, _ := slack.New(pgtest.NewID("nch"), a.ID, now)
	cross.Name, cross.SecretID = "cross", secB
	_ = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		if err := repo.Insert(ctx, tx, cross); err == nil {
			t.Error("another team's secret accepted")
		}
		return context.Canceled
	})

	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		rows, err := repo.List(ctx, tx, a.ID, "", 10)
		if err != nil || len(rows) != 2 {
			t.Fatalf("list: %d %v", len(rows), err)
		}
		got, err := repo.Get(ctx, tx, a.ID, slackC.ID, false)
		if err != nil || got.SecretID != secA || got.Status.Phase != domain.PhasePending || got.Config["channel"] != "#alerts" || len(got.EventTypes) != 1 {
			t.Fatalf("get: %+v %v", got, err)
		}
		if ok, _ := repo.SecretExists(ctx, tx, a.ID, secB); ok {
			t.Fatal("another team's secret is visible")
		}
		if _, err = repo.Get(ctx, tx, a.ID, foreign.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Fatalf("foreign channel: %v", err)
		}
		// A service channel is not a team channel, and a team caller sees service rows only as the service scope.
		if _, err = repo.Get(ctx, tx, a.ID, svc.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Fatalf("service channel through the team scope: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := inTx(t, db, teamSession(b.ID), func(ctx context.Context, tx port.Tx) error {
		if rows, _ := repo.List(ctx, tx, a.ID, "", 10); len(rows) != 0 {
			t.Fatalf("team B sees %d channels of team A", len(rows))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Service scope: a service admin lists the service channels only.
	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		rows, err := repo.List(ctx, tx, "", "", 10)
		if err != nil || len(rows) != 1 || rows[0].ID != svc.ID || !rows[0].ServiceLevel() {
			t.Fatalf("service list: %+v %v", rows, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Update: stale refused, the trigger bumps the version, a missing id is NotFound.
	off := false
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		next, _ := mailC.Apply(domain.ChannelPatch{Enabled: &off}, now)
		_, err := repo.Update(ctx, tx, next, 9)
		wantKind(t, err, domain.KindPreconditionFailed)
		up, err := repo.Update(ctx, tx, next, 1)
		if err != nil || up.Version != 2 || up.Enabled {
			t.Fatalf("update: %+v %v", up, err)
		}
		missing := next
		missing.ID = pgtest.NewID("nch")
		_, err = repo.Update(ctx, tx, missing, 1)
		wantKind(t, err, domain.KindNotFound)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The setting lookup.
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.service_settings (key, value) VALUES ('work_orders.notification_channel_id', to_jsonb($1::text))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, svc.ID); err != nil {
		t.Fatal(err)
	}
	_ = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		if named, err := repo.NamedByWorkOrderSetting(ctx, tx, svc.ID); err != nil || !named {
			t.Fatalf("named: %v %v", named, err)
		}
		if named, _ := repo.NamedByWorkOrderSetting(ctx, tx, slackC.ID); named {
			t.Fatal("an unrelated channel is not named")
		}
		return nil
	})
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.Delete(ctx, tx, a.ID, slackC.ID, 7), domain.KindPreconditionFailed)
		if err := repo.Delete(ctx, tx, a.ID, slackC.ID, 1); err != nil {
			t.Fatal(err)
		}
		wantKind(t, repo.Delete(ctx, tx, a.ID, slackC.ID, 1), domain.KindNotFound)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type chanListerBase struct{ listed []engine.Kind }

func (f *chanListerBase) List(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	f.listed = append(f.listed, kinds...)
	return []engine.Ref{engine.NewRef(engine.KindTeam, "base", "base")}, nil
}

func (f *chanListerBase) Stale(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	f.listed = append(f.listed, kinds...)
	return nil, nil
}

// M3-G3, M3-G4: the reconciler's source, lister, status recorder, failure event and test requests.
func TestChannelReconcilerSide(t *testing.T) {
	db := pgtest.Get(t)
	tm := db.SeedTeam(t)
	sec := teamSecret(t, db, tm.ID, "slack")
	admin := db.Admin(t)
	slackID, svcID := pgtest.NewID("nch"), pgtest.NewID("nch")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO afs.notification_channels (id, team_id, name, kind, config, secret_id, event_types) VALUES ($1, $2, 'slack', 'slack', '{"channel":"#a1"}', $3, '{budget.*}')`, []any{slackID, tm.ID, sec}},
		{`INSERT INTO afs.notification_channels (id, team_id, name, kind, config) VALUES ($1, NULL, 'svc', 'email', '{"to":["a@example.com"]}')`, []any{svcID}},
	} {
		if _, err := admin.Exec(t.Context(), q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	// Other tests of this package count unpublished outbox rows: leave none behind.
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.outbox_events WHERE team_id = $1`, tm.ID)
	})
	rr := reconcilerPool(t, db)
	src := postgres.NewChannelSource(rr)
	ch, err := src.LoadChannel(t.Context(), slackID)
	if err != nil || ch.TeamID != tm.ID || ch.TeamState != "active" || ch.SecretARN == "" || ch.Kind != domain.ChannelSlack || ch.Version != 1 ||
		len(ch.EventTypes) != 1 || ch.Config["channel"] != "#a1" || len(ch.Prior) != 0 {
		t.Fatalf("%+v %v", ch, err)
	}
	if s, err := src.LoadChannel(t.Context(), svcID); err != nil || s.TeamID != "" || s.TeamState != "active" || s.SecretARN != "" {
		t.Fatalf("service channel: %+v %v", s, err)
	}
	if _, err := src.LoadChannel(t.Context(), pgtest.NewID("nch")); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("unknown: %v", err)
	}

	// The lister: both channels, the base's refs, and stale until their version is observed.
	base := &chanListerBase{}
	l := postgres.NewChannelLister(base, admin)
	refs, err := l.List(t.Context(), []engine.Kind{engine.KindTeam, notify.Kind})
	if err != nil {
		t.Fatal(err)
	}
	var gotSlack, gotSvc bool
	for _, r := range refs {
		if r.Kind == notify.Kind && r.ID == slackID && r.TeamID == tm.ID {
			gotSlack = true
		}
		if r.Kind == notify.Kind && r.ID == svcID && r.TeamID == "" {
			gotSvc = true
		}
	}
	if !gotSlack || !gotSvc || len(base.listed) != 1 || base.listed[0] != engine.KindTeam {
		t.Fatalf("refs=%v listed=%v", refs, base.listed)
	}
	stale, err := l.Stale(t.Context(), []engine.Kind{notify.Kind})
	if err != nil || len(stale) < 2 {
		t.Fatalf("stale before status: %v %v", stale, err)
	}

	// Status: a failure is recorded and merged with the history; a success after it flips health.
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	runner := rr
	status := postgres.NewStatusWriter(runner, postgres.NewOutboxWriter(), &fakes.IDGen{}, &fakes.Clock{T: now})
	cs := postgres.NewChannelStatus(runner, status, func() time.Time { return now })
	if err := cs.Record(t.Context(), ch, notify.Delivery{At: now, Err: "HTTP 404 no_service", TestID: "ntest_1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := src.LoadChannel(t.Context(), slackID)
	d := domain.DeliveryDetailOf(got.Prior)
	if h, _ := (domain.NotificationChannel{Status: domain.Status{Conditions: got.Prior}}).Status.Condition(domain.CondDeliveryHealthy); h.Status != domain.ConditionFalse ||
		d.LastTestID != "ntest_1" || d.TestDelivered == nil || *d.TestDelivered || d.LastError != "HTTP 404 no_service" {
		t.Fatalf("%+v %+v", got.Prior, d)
	}
	if err := cs.Record(t.Context(), ch, notify.Delivery{At: now.Add(time.Minute), OK: true}); err != nil {
		t.Fatal(err)
	}
	got, _ = src.LoadChannel(t.Context(), slackID)
	d = domain.DeliveryDetailOf(got.Prior)
	if h, _ := (domain.NotificationChannel{Status: domain.Status{Conditions: got.Prior}}).Status.Condition(domain.CondDeliveryHealthy); h.Status != domain.ConditionTrue ||
		d.LastDeliveredAt == nil || d.LastError == "" {
		t.Fatalf("history kept after a success: %+v", d)
	}
	// Deleting the status row removes it (the channel is gone).
	if err := cs.DeleteStatus(t.Context(), slackID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.resource_status WHERE resource_kind = 'notification_channel' AND resource_id = $1`, slackID).Scan(&n)
	if n != 0 {
		t.Fatalf("status rows = %d", n)
	}

	// The failure event: one outbox row of type notification.delivery_failed with the channel in the data.
	ev := postgres.NewChannelEvents(runner, postgres.NewOutboxWriter(), &fakes.IDGen{}, func() time.Time { return now })
	if err := ev.DeliveryFailed(t.Context(), ch, "HTTP 404", "budget.threshold_crossed"); err != nil {
		t.Fatal(err)
	}
	_ = admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'notification.delivery_failed' AND team_id = $1 AND payload #>> '{data,channelId}' = $2`, tm.ID, slackID).Scan(&n)
	if n != 1 {
		t.Fatalf("delivery_failed events = %d", n)
	}

	// Test requests: only notification.test_requested rows inside the window.
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.outbox_events (event_type, aggregate_kind, aggregate_id, team_id, payload) VALUES
('notification.test_requested', 'notification_channels', $1, $2, jsonb_build_object('data', jsonb_build_object('channelId', $1::text, 'testId', 'ntest_abc'))),
('team.created', 'team', $2, $2, '{}')`, slackID, tm.ID); err != nil {
		t.Fatal(err)
	}
	reqs, err := postgres.NewChannelTests(runner).PendingTests(t.Context(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range reqs {
		found = found || (r.ChannelID == slackID && r.TestID == "ntest_abc")
	}
	if !found {
		t.Fatalf("requests: %+v", reqs)
	}
	if old, _ := postgres.NewChannelTests(runner).PendingTests(t.Context(), time.Now().Add(time.Hour)); len(old) != 0 {
		t.Fatalf("future window returned %+v", old)
	}
}

// lifecycleBus is an in-memory notify.Bus for the engine test.
type lifecycleBus struct {
	mu    sync.Mutex
	rules map[string]notify.RuleSpec
}

func (b *lifecycleBus) Apply(_ context.Context, s notify.RuleSpec) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rules[s.Channel.ID] = s
	return []string{"rule/" + s.Channel.ID}, nil
}

func (b *lifecycleBus) Remove(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.rules, id)
	return nil
}

func (b *lifecycleBus) List(context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for id := range b.rules {
		ids = append(ids, id)
	}
	return ids, nil
}

func (b *lifecycleBus) rule(id string) (notify.RuleSpec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.rules[id]
	return s, ok
}

type noSecrets struct{}

func (noSecrets) Read(context.Context, string) (notify.Credential, error) { return "", nil }

type emptyLister struct{}

func (emptyLister) List(context.Context, []engine.Kind) ([]engine.Ref, error)  { return nil, nil }
func (emptyLister) Stale(context.Context, []engine.Kind) ([]engine.Ref, error) { return nil, nil }

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// RC-US-13.1, RC-US-13.4: through the real change feed, a channel row becomes a rule filtered on its
// team, a change updates it, and a delete removes the rule and the status row well inside 60 s; a
// disabled channel has no rule.
func TestChannelRuleLifecycleThroughTheEngine(t *testing.T) {
	db := pgtest.Get(t)
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	admin := db.Admin(t)
	rr := reconcilerPool(t, db)
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_reconciler"], Service: "afs-reconciler", SystemActor: true, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	bus := &lifecycleBus{rules: map[string]notify.RuleSpec{}}
	now := time.Now
	status := postgres.NewStatusWriter(rr, postgres.NewOutboxWriter(), &fakes.IDGen{}, &fakes.Clock{T: time.Now()})
	cs := postgres.NewChannelStatus(rr, status, now)
	ctl, err := notify.NewController(notify.Config{Source: postgres.NewChannelSource(rr), Bus: bus, Secrets: noSecrets{}, Cleaner: cs, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := engine.NewRegistry(notify.Route())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e, err := engine.New(engine.Config{
		Feed:   postgres.NewChangeFeed(postgres.Config{URL: db.URLs["afs_reconciler"], Service: "afs-reconciler", SystemActor: true}, log),
		Lister: postgres.NewChannelLister(emptyLister{}, pool), Registry: reg, Controllers: []engine.Controller{ctl}, Status: status,
		PollInterval: 200 * time.Millisecond, ResyncInterval: time.Minute, Now: now, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	id := pgtest.NewID("nch")
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.notification_channels (id, team_id, name, kind, config, event_types) VALUES ($1, $2, 'mail', 'email', '{"to":["a@example.com"]}', '{budget.*}')`, id, a.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rule", 10*time.Second, func() bool { _, ok := bus.rule(id); return ok })
	spec, _ := bus.rule(id)
	if !strings.Contains(spec.Pattern, a.ID) || strings.Contains(spec.Pattern, b.ID) {
		t.Fatalf("a team's rule must name that team only: %s", spec.Pattern)
	}
	var phase string
	eventually(t, "the status row", 10*time.Second, func() bool {
		return admin.QueryRow(t.Context(), `SELECT phase FROM afs.resource_status WHERE resource_kind = 'notification_channel' AND resource_id = $1`, id).Scan(&phase) == nil
	})
	if phase != "Ready" {
		t.Fatalf("phase = %s", phase)
	}

	// A change reaches the rule; disabling removes it.
	if _, err := admin.Exec(t.Context(), `UPDATE afs.notification_channels SET event_types = '{agent.*}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the updated pattern", 10*time.Second, func() bool { s, _ := bus.rule(id); return strings.Contains(s.Pattern, `"agent.`) })
	if _, err := admin.Exec(t.Context(), `UPDATE afs.notification_channels SET enabled = false WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rule to go while disabled", 10*time.Second, func() bool { _, ok := bus.rule(id); return !ok })
	if _, err := admin.Exec(t.Context(), `UPDATE afs.notification_channels SET enabled = true WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rule to return", 10*time.Second, func() bool { _, ok := bus.rule(id); return ok })

	// Delete: the rule and the status row go (the 60 s requirement, with seconds to spare).
	start := time.Now()
	if _, err := admin.Exec(t.Context(), `DELETE FROM afs.notification_channels WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the rule to be removed", 20*time.Second, func() bool { _, ok := bus.rule(id); return !ok })
	eventually(t, "the status row to be removed", 20*time.Second, func() bool {
		var n int
		_ = admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.resource_status WHERE resource_kind = 'notification_channel' AND resource_id = $1`, id).Scan(&n)
		return n == 0
	})
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("removal took %s", d)
	}
}
