package postgres_test

import (
	"context"
	"strings"
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
	exec(`UPDATE afs.agent_templates SET capabilities = ARRAY['orchestration','tool:paperclip'] WHERE name = $1`, tpl)
	collab, worker := pgtest.NewID("agt"), pgtest.NewID("agt")
	img := "registry.example.com/agents/coder@" + testDigest
	exec(`INSERT INTO afs.agents (id, team_id, name, template, template_id, image, model_id, role, iam_identity)
VALUES ($1, $2, 'pc', $3, (SELECT id FROM afs.agent_templates WHERE name = $3), $4, $5, 'orchestration', 'dedicated')`, collab, tm.ID, tpl, img, model)
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
	if st == nil || st.AgentID != collab || st.TeamSlug != tm.Slug || st.PodReady || st.Mode != domain.RegistrationManual || st.WebUIExposure != domain.ExposurePublic {
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
	exec(`INSERT INTO afs.service_settings (key, value) VALUES ('orchestration.public_base_domain', '"collab.example.com"')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.service_settings WHERE key = 'orchestration.public_base_domain'`)
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

// RC-25, COL-12, COL-32, COL-33: the managed-mode adapters read the managed teams and their links, write the
// observed link and the key records (through the SECURITY DEFINER functions), find the people to invite and
// the creator of the collaboration agent.
func TestCollabManagedAdapters(t *testing.T) {
	db := pgtest.Get(t)
	// The guard trigger on agent_collaboration keys on current_user = 'afs_reconciler' (the production login
	// is that role); the test's login is a member of it, so the session takes the role explicitly.
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_reconciler"] + "&options=-c%20role%3Dafs_reconciler", Service: "afs-reconciler", SystemActor: true, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	runner := postgres.NewTxRunner(pool)
	admin := db.Admin(t)
	ctx := context.Background()
	tm := db.SeedTeam(t)
	model := seedModel(t, db, tm.ID)
	tpl := "pc-" + lower(pgtest.NewID("x")[2:8])
	seedTemplateForAgents(t, db, tpl)
	wtpl := "wk-" + lower(pgtest.NewID("x")[2:8])
	seedTemplateForAgents(t, db, wtpl)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE afs.agent_templates SET capabilities = ARRAY['orchestration','tool:paperclip'] WHERE name = $1`, tpl)
	collab, worker := pgtest.NewID("agt"), pgtest.NewID("agt")
	creator, viewer, signedIn := pgtest.NewID("usr"), pgtest.NewID("usr"), pgtest.NewID("usr")
	for i, u := range []string{creator, viewer, signedIn} {
		exec(`INSERT INTO afs.users (id, oidc_issuer, oidc_subject, email, display_name) VALUES ($1, 'iss', $1, $2, 'U')`, u, []string{"Boss@Example.com", "view@example.com", "grp@example.com"}[i]+"."+u[4:10])
	}
	img := "registry.example.com/agents/coder@" + testDigest
	exec(`INSERT INTO afs.agents (id, team_id, name, template, template_id, image, model_id, role, iam_identity, created_by)
VALUES ($1, $2, 'pc', $3, (SELECT id FROM afs.agent_templates WHERE name = $3), $4, $5, 'orchestration', 'dedicated', $6)`, collab, tm.ID, tpl, img, model, creator)
	exec(`INSERT INTO afs.agents (id, team_id, name, template, image, model_id, persona) VALUES ($1, $2, 'wk', $3, $4, $5, 'helps')`, worker, tm.ID, wtpl, img, model)
	exec(`INSERT INTO afs.team_collaboration (team_id, agent_id, registration_mode) VALUES ($1, $2, 'managed')`, tm.ID, collab)
	exec(`INSERT INTO afs.agent_collaboration (team_id, agent_id, connect) VALUES ($1, $2, true)`, tm.ID, worker)
	exec(`INSERT INTO afs.role_bindings (id, role, team_id, subject_type, user_id) VALUES ($1, 'team_admin', $3, 'user', $4), ($2, 'team_viewer', $3, 'user', $5)`,
		pgtest.NewID("rb"), pgtest.NewID("rb"), tm.ID, creator, viewer)
	a := postgres.NewCollabReconcile(runner, postgres.NewOutboxWriter(), &fakes.IDGen{}, &fakes.Clock{T: time.Now()}, nil)

	// No board key yet: the team is not a managed team the registrar may call.
	mts, err := a.ManagedTeams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mts {
		if m.TeamID == tm.ID {
			t.Fatalf("team without a board key listed: %+v", m)
		}
	}
	// Record the board key at the agent's path.
	bpath, err := a.SecretPath(ctx, tm.ID, collab, domain.SecretPaperclipBoardAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	barn := "arn:aws:secretsmanager:us-east-1:111122223333:secret:" + bpath + "-AbCdEf"
	if err := a.Record(ctx, tm.ID, collab, barn); err != nil {
		t.Fatal(err)
	}
	if tg, err := a.PaperclipTarget(ctx, tm.ID); err != nil || tg.BoardKeyARN != barn || tg.Namespace == "" {
		t.Errorf("target: %+v %v", tg, err)
	}
	if _, err := a.PaperclipTarget(ctx, "team_nonexistent"); err == nil {
		t.Error("unknown team")
	}
	pend, _ := a.Pending(ctx)
	for _, p := range pend {
		if p.TeamID == tm.ID && (!p.Recorded || p.PodReady) {
			t.Errorf("candidate: %+v", p)
		}
	}
	mts, err = a.ManagedTeams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mt *rcollab.ManagedTeam
	for i := range mts {
		if mts[i].TeamID == tm.ID {
			mt = &mts[i]
		}
	}
	if mt == nil || mt.CollabAgentID != collab || mt.PodReady || mt.Gone || len(mt.Links) != 1 {
		t.Fatalf("managed team: %+v", mt)
	}
	l := mt.Links[0]
	wpath, _ := a.SecretPath(ctx, tm.ID, worker, domain.SecretPaperclipAgentAPIKey)
	if l.AgentID != worker || l.AgentName != "wk" || l.Persona != "helps" || !l.Connect || l.Paused || l.KeyARN != "" || l.KeyPath != wpath || l.LinkState != domain.LinkPending {
		t.Errorf("link: %+v", l)
	}

	// Key records through the SECURITY DEFINER functions: the secret row (managed_by afs) and the binding.
	warn := "arn:aws:secretsmanager:us-east-1:111122223333:secret:" + wpath + "-ZyXwVu"
	if err := a.RecordKey(ctx, tm.ID, worker, "arn:aws:secretsmanager:us-east-1:111122223333:secret:elsewhere-ZyXwVu"); err == nil {
		t.Error("an ARN outside the worker's path was accepted")
	}
	if err := a.RecordKey(ctx, tm.ID, worker, warn); err != nil {
		t.Fatal(err)
	}
	var managedBy, target string
	if err := admin.QueryRow(ctx, `SELECT s.managed_by, b.target FROM afs.secrets s JOIN afs.agent_secret_bindings b ON b.secret_id = s.id WHERE s.agent_id = $1`, worker).Scan(&managedBy, &target); err != nil || managedBy != "afs" || target != "PAPERCLIP_AGENT_API_KEY" {
		t.Errorf("records: %s %s %v", managedBy, target, err)
	}
	if err := a.SetManaged(ctx, tm.ID, worker, rcollab.ManagedUpdate{State: domain.LinkLinked, PaperclipAgentID: "pc-9", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var state, pcid string
	var synced *time.Time
	if err := admin.QueryRow(ctx, `SELECT link_state, coalesce(paperclip_agent_id, ''), last_synced_at FROM afs.agent_collaboration WHERE agent_id = $1`, worker).Scan(&state, &pcid, &synced); err != nil || state != "linked" || pcid != "pc-9" || synced == nil {
		t.Errorf("link row: %s %s %v %v", state, pcid, synced, err)
	}
	mts, _ = a.ManagedTeams(ctx)
	for _, m := range mts {
		if m.TeamID == tm.ID && (m.Links[0].KeyARN != warn || m.Links[0].PaperclipAgentID != "pc-9") {
			t.Errorf("link after record: %+v", m.Links[0])
		}
	}
	// Failures clear the id and keep a cut error.
	if err := a.SetManaged(ctx, tm.ID, worker, rcollab.ManagedUpdate{State: domain.LinkFailed, LastError: strings.Repeat("e", 600), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var lerr string
	if err := admin.QueryRow(ctx, `SELECT coalesce(paperclip_agent_id, ''), last_error FROM afs.agent_collaboration WHERE agent_id = $1`, worker).Scan(&pcid, &lerr); err != nil || pcid != "" || len(lerr) != 500 {
		t.Errorf("failed row: %q %d %v", pcid, len(lerr), err)
	}
	if ok, err := a.RemoveKey(ctx, tm.ID, worker); err != nil || !ok {
		t.Errorf("remove: %v %v", ok, err)
	}
	if ok, err := a.RemoveKey(ctx, tm.ID, worker); err != nil || ok {
		t.Errorf("second remove: %v %v", ok, err)
	}

	// The creator's email (a person), and none for an agent created by a service.
	if em, err := a.CreatorEmail(ctx, tm.ID, collab); err != nil || !strings.HasPrefix(em, "Boss@Example.com.") {
		t.Errorf("creator: %q %v", em, err)
	}
	if em, err := a.CreatorEmail(ctx, tm.ID, worker); err != nil || em != "" {
		t.Errorf("service creator: %q %v", em, err)
	}

	// People: bindings and recent edge sign-ins, only for teams with a ready pod.
	if ppl, err := a.People(ctx); err != nil || hasTeam(ppl, tm.ID) {
		t.Fatalf("not ready: %v %v", ppl, err)
	}
	exec(`INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase) VALUES ('agent', $1, $2, 'Ready')`, collab, tm.ID)
	if err := rcollab.EnsurePartitions(ctx, a, time.Now); err != nil {
		t.Fatal(err)
	}
	bucket := time.Now().UTC().Truncate(5 * time.Minute)
	exec(`INSERT INTO afs.collaboration_access (bucket_start, team_id, surface, principal_kind, principal_id, outcome, requests)
VALUES ($1, $2, 'web_ui', 'user', $3, 'signed_in', 1), ($1, $2, 'web_ui', 'user', $4, 'signed_in', 1)`, bucket, tm.ID, signedIn, viewer)
	ppl, err := a.People(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]rcollab.InviteRole{}
	for _, tp := range ppl {
		if tp.TeamID == tm.ID {
			for _, p := range tp.Wanted {
				roles[p.Email[:strings.Index(p.Email, "@")]] = p.Role
			}
		}
	}
	// A binding as viewer wins over nothing, and a sign-in adds member: DISTINCT ON prefers member.
	if len(roles) != 3 || roles["boss"] != rcollab.RoleMember || roles["grp"] != rcollab.RoleMember || roles["view"] != rcollab.RoleMember {
		t.Errorf("people: %v", roles)
	}

	// A team that lost its collaboration agent: its link with records is returned as a Gone team.
	if err := a.SetManaged(ctx, tm.ID, worker, rcollab.ManagedUpdate{State: domain.LinkLinked, PaperclipAgentID: "pc-left", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM afs.team_collaboration WHERE team_id = $1`, tm.ID)
	mts, _ = a.ManagedTeams(ctx)
	var gone *rcollab.ManagedTeam
	for i := range mts {
		if mts[i].TeamID == tm.ID {
			gone = &mts[i]
		}
	}
	if gone == nil || !gone.Gone || !gone.PodReady || len(gone.Links) != 1 || !gone.Links[0].CollabGone {
		t.Errorf("gone team: %+v", gone)
	}
}

func hasTeam(ppl []rcollab.TeamPeople, id string) bool {
	for _, p := range ppl {
		if p.TeamID == id {
			return true
		}
	}
	return false
}
