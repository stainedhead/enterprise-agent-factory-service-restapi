package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	rcollab "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

// RC-21..29: the reconciler's collaboration adapters read the states and links, write link state, count denied
// requests, publish events, and call the housekeeping functions, as the reconciler role.
func TestCollabReconcile(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	admin := db.Admin(t)
	ctx := context.Background()
	tm := db.SeedTeam(t)
	model := seedModel(t, db, tm.ID)
	tpl := "pc-" + lower(pgtest.NewID("x")[2:8])
	tplID := seedTemplateForAgents(t, db, tpl)
	wtpl := "wk-" + lower(pgtest.NewID("x")[2:8])
	wtplID := seedTemplateForAgents(t, db, wtpl)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE afs.agent_templates SET capabilities = ARRAY['paperclip'] WHERE name = $1`, tpl)
	collab, worker := pgtest.NewID("agt"), pgtest.NewID("agt")
	img := "registry.example.com/agents/coder@" + testDigest
	exec(`INSERT INTO afs.agents (id, team_id, name, template, template_id, image, model_id, role, iam_identity)
VALUES ($1, $2, 'pc', $3, (SELECT id FROM afs.agent_templates WHERE name = $3), $4, $5, 'collaboration', 'dedicated')`, collab, tm.ID, tpl, img, model)
	exec(`INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, 'wk', $3, $4, $5)`, worker, tm.ID, wtpl, img, model)
	exec(`INSERT INTO afs.team_collaboration (team_id, agent_id) VALUES ($1, $2)`, tm.ID, collab)
	exec(`INSERT INTO afs.agent_collaboration (team_id, agent_id, connect, paperclip_agent_id) VALUES ($1, $2, true, 'pc-1')`, tm.ID, worker)

	outbox := postgres.NewOutboxWriter()
	a := postgres.NewCollabReconcile(runner, outbox, &fakes.IDGen{}, &fakes.Clock{T: time.Now()}, nil)

	states, err := a.States(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var st *rcollab.TeamState
	for i := range states {
		if states[i].TeamID == tm.ID {
			st = &states[i]
		}
	}
	if st == nil || st.AgentID != collab || st.TeamSlug != tm.Slug || st.PodReady || st.Mode != domain.RegistrationManual || !st.WebUIEnabled {
		t.Fatalf("state: %+v", st)
	}

	links, err := a.Links(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var link *rcollab.Link
	for i := range links {
		if links[i].AgentID == worker {
			link = &links[i]
		}
	}
	if link == nil || !link.Connect || link.PaperclipAgentID != "pc-1" || link.KeyPresent || link.AgentGone || link.CollabAgentGone {
		t.Fatalf("link: %+v", link)
	}
	if err := a.SetState(ctx, tm.ID, worker, domain.LinkFailed, "boom", time.Now()); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := admin.QueryRow(ctx, `SELECT link_state, last_error FROM afs.agent_collaboration WHERE agent_id = $1`, worker).Scan(&state, &lastErr); err != nil || state != "failed" || lastErr != "boom" {
		t.Errorf("stored: %s %q %v", state, lastErr, err)
	}

	// The partitions come first: a row already in the default partition blocks creating its month (a -db follow-up).
	if err := rcollab.EnsurePartitions(ctx, a, time.Now); err != nil {
		t.Fatal(err)
	}
	// Denied counts come from the five-minute buckets; the threshold is the setting's default.
	bucket := time.Now().UTC().Truncate(5 * time.Minute)
	exec(`INSERT INTO afs.collaboration_access (bucket_start, team_id, surface, principal_kind, principal_id, outcome, requests)
VALUES ($1, $2, 'rest_api', 'anonymous', '', 'denied_unauthenticated', 7), ($1, $2, 'rest_api', 'anonymous', '', 'allowed', 100)`, bucket, tm.ID)
	denied, err := a.DeniedSince(ctx, bucket.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var got rcollab.Denied
	for _, d := range denied {
		if d.TeamID == tm.ID {
			got = d
		}
	}
	if got.Unauthenticated != 7 || got.Total() != 7 {
		t.Errorf("denied: %+v", got)
	}
	if n, err := a.Threshold(ctx); err != nil || n <= 0 {
		t.Errorf("threshold: %d %v", n, err)
	}
	if last, err := a.LastSpike(ctx, tm.ID); err != nil || !last.IsZero() {
		t.Errorf("last spike: %v %v", last, err)
	}
	if err := a.Publish(ctx, domain.EventCollaborationDeniedSpike, tm.ID, "team_collaboration", tm.ID, "",
		map[string]any{"teamId": tm.ID, "windowStart": bucket, "windowEnd": bucket.Add(5 * time.Minute), "denied": 60, "threshold": 50,
			"byReason": map[string]any{"unauthenticated": 60, "forbidden": 0, "surface_disabled": 0, "secret_detected": 0}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.outbox_events WHERE team_id = $1`, tm.ID)
	})
	if last, err := a.LastSpike(ctx, tm.ID); err != nil || last.IsZero() {
		t.Errorf("last spike after publish: %v %v", last, err)
	}

	if cidrs, err := a.AllowedCIDRs(ctx); err != nil || len(cidrs) != 0 {
		t.Errorf("allowed cidrs: %v %v", cidrs, err)
	}

	// The agent source reads the role, the collaboration facts and a connected worker's link (RC-21, RC-22).
	src := postgres.NewAgentSource(runner)
	exec(`INSERT INTO afs.service_settings (key, value) VALUES ('collab.base_domain', '"collab.example.com"')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.service_settings WHERE key = 'collab.base_domain'`)
	})
	ca, err := src.LoadAgent(ctx, collab)
	if err != nil || !ca.IsCollaboration() || ca.Collab == nil || ca.Collab.Host != tm.Slug+".collab.example.com" || ca.Collab.Mode != "manual" || ca.Link != nil {
		t.Errorf("collaboration agent: %+v %v", ca.Collab, err)
	}
	wa, err := src.LoadAgent(ctx, worker)
	if err != nil || wa.IsCollaboration() || wa.Collab != nil || wa.Link == nil || wa.Link.PaperclipAgentID != "pc-1" {
		t.Errorf("connected worker: %+v %v", wa.Link, err)
	}
	exec(`UPDATE afs.agent_collaboration SET connect = false WHERE agent_id = $1`, worker)
	if wa, err = src.LoadAgent(ctx, worker); err != nil || wa.Link != nil {
		t.Errorf("disconnected worker: %+v %v", wa.Link, err)
	}
	exec(`UPDATE afs.agent_collaboration SET connect = true WHERE agent_id = $1`, worker)

	// RA-22: only an enabled paperclip template may be a swarm's collaboration agent; nothing references it yet.
	if err := inTx(t, db, port.SystemSession("test"), func(ctx context.Context, tx port.Tx) error {
		sw, at := postgres.NewSwarmTemplateRepo(), postgres.NewAgentTemplateRepo()
		if ok, err := sw.CollaborationTemplateOK(ctx, tx, tplID); err != nil || !ok {
			t.Errorf("paperclip template: %v %v", ok, err)
		}
		if ok, err := sw.CollaborationTemplateOK(ctx, tx, wtplID); err != nil || ok {
			t.Errorf("plain template: %v %v", ok, err)
		}
		if used, err := at.ReferencedBySwarm(ctx, tx, tplID); err != nil || used {
			t.Errorf("referenced: %v %v", used, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Housekeeping and the board key bootstrap.
	if _, err := a.PurgeEdgeSessions(ctx); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE afs.team_collaboration SET registration_mode = 'managed' WHERE team_id = $1`, tm.ID)
	pend, err := a.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range pend {
		found = found || p.TeamID == tm.ID
	}
	if !found {
		t.Error("managed team without a board key is not pending")
	}
	if _, ok, err := a.Find(ctx, tm.ID, collab); err != nil || ok {
		t.Errorf("find without a finder: %v %v", ok, err)
	}
	if err := a.Record(ctx, tm.ID, collab, "arn:aws:secretsmanager:us-east-1:111122223333:secret:wrong-AbCdEf"); err == nil {
		t.Error("a board key ARN outside the agent's path was accepted")
	}
}
