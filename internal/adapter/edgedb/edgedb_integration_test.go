package edgedb_test

import (
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/edgedb"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// EDGE-2..7: the edge adapter reads routes, settings and principals, keeps sessions and counts access, all
// through the ten functions the afs_edge role may execute.
func TestEdgeDB(t *testing.T) {
	db := pgtest.Get(t)
	ctx := t.Context()
	admin := db.Admin(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	tm := db.SeedTeam(t)
	pool, err := postgres.OpenPool(ctx, postgres.Config{URL: db.URLs["afs_edge"], Service: "afs-edge", SystemActor: true, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	e := edgedb.New(pool)

	// the role has no table privilege
	if _, err := pool.Exec(ctx, `SELECT 1 FROM afs.teams LIMIT 1`); err == nil {
		t.Fatal("afs_edge must not read tables")
	}

	// settings: defaults, then overrides (wrong types keep the default)
	set, err := e.Settings(ctx)
	if err != nil || set.ContentScan != "block" || set.MaxBodyMB != 25 || set.BaseDomain != "" {
		t.Fatalf("defaults: %+v %v", set, err)
	}
	for k, v := range map[string]string{"collab.base_domain": `"collab.example.com"`, "collab.content_scan": `"warn"`, "collab.max_body_mb": `10`,
		"collab.allowed_source_cidrs": `["10.0.0.0/8"]`, "collab.session_ttl_minutes": `60`, "collab.denied_spike_threshold": `7`, "collab.public_endpoints": `true`} {
		exec(`INSERT INTO afs.service_settings (key, value) VALUES ($1, $2::jsonb) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, k, v)
	}
	if set, err = e.Settings(ctx); err != nil || set.BaseDomain != "collab.example.com" || set.ContentScan != "warn" || set.MaxBodyMB != 10 ||
		len(set.AllowedSourceCIDRs) != 1 || set.SessionTTLMinutes != 60 || set.DeniedSpikeThreshold != 7 {
		t.Fatalf("settings: %+v %v", set, err)
	}

	// routes
	idp, agent := pgtest.NewID("idp"), pgtest.NewID("agt")
	exec(`INSERT INTO afs.identity_providers (id, kind, name, issuer_url, client_id, collab_client_id, collab_client_secret_arn, is_default)
VALUES ($1, 'okta', 'Okta', 'https://idp.example.com', 'cid', 'ccid', 'arn:aws:secretsmanager:eu-west-1:000000000000:secret:afs/x-abcdef', true)`, idp)
	tpl := "pc-" + strings.ToLower(pgtest.NewID("x")[2:8])
	digest := "sha256:" + strings.Repeat("a", 64)
	exec(`INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest)
VALUES ($1, $2, 'hermes', ARRAY[$3]::text[], $3)`, pgtest.NewID("tpl"), tpl, digest)
	_ = agent
	routes, err := e.Routes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.TeamID == tm.ID {
			t.Fatalf("a team without collaboration has no route: %+v", r)
		}
	}

	// principal and key
	if b, err := e.Person(ctx, tm.ID, "nobody", idp, nil); err != nil || len(b) != 0 {
		t.Fatalf("unknown person: %v %v", b, err)
	}
	if _, err := e.Person(ctx, tm.ID, "x", idp, make([]string, 501)); err == nil {
		t.Error("more than 500 groups must be refused")
	}
	exec(`INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name) VALUES ($1, 'team_viewer', $2, 'group', 'eng')`, pgtest.NewID("rb"), tm.ID)
	if b, err := e.Person(ctx, tm.ID, "x", idp, []string{"eng"}); err != nil || len(b) != 1 || b[0].Role != "team_viewer" {
		t.Fatalf("group binding: %v %v", b, err)
	}
	sum := sha256.Sum256([]byte("afsk_test"))
	exec(`INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES ($1, $2, 'k', 'team_admin', 'afsk_test', $3, now() + interval '30 days')`,
		pgtest.NewID("key"), tm.ID, sum[:])
	k, ok, err := e.Key(ctx, tm.ID, sum)
	if err != nil || !ok || k.Role != "team_admin" || k.State != "active" {
		t.Fatalf("key: %+v %v %v", k, ok, err)
	}
	if _, ok, err := e.Key(ctx, tm.ID, sha256.Sum256([]byte("other"))); err != nil || ok {
		t.Fatalf("unknown key: %v %v", ok, err)
	}
	if _, ok, _ := e.Key(ctx, pgtest.NewID("team"), sum); ok {
		t.Fatal("a key of another team must not resolve")
	}
	exec(`UPDATE afs.api_keys SET revoked_at = now() WHERE team_id = $1`, tm.ID)
	if k, _, _ := e.Key(ctx, tm.ID, sum); k.State != "revoked" {
		t.Fatalf("revoked: %+v", k)
	}

	// handoff and session need a collaboration row
	collabAgent := pgtest.NewID("agt")
	const model = "anthropic.claude-edge-test"
	exec(`INSERT INTO afs.models (id, provider, display_name, max_output_tokens) VALUES ($1, 'anthropic', 'edge', 4096) ON CONFLICT DO NOTHING`, model)
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, tm.ID, model)
	exec(`UPDATE afs.agent_templates SET capabilities = ARRAY['paperclip'] WHERE name = $1`, tpl)
	exec(`INSERT INTO afs.agents (id, team_id, name, template, template_id, image, model_id, role, iam_identity)
VALUES ($1, $2, 'pc', $3, (SELECT id FROM afs.agent_templates WHERE name = $3), $4, $5, 'collaboration', 'dedicated')`,
		collabAgent, tm.ID, tpl, "registry.example.com/agents/coder@"+digest, model)
	exec(`INSERT INTO afs.team_collaboration (team_id, agent_id) VALUES ($1, $2)`, tm.ID, collabAgent)
	routes, err = e.Routes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var route *collab.Route
	for i := range routes {
		if routes[i].TeamID == tm.ID {
			route = &routes[i]
		}
	}
	if route == nil || route.Hostname != tm.Slug+".collab.example.com" || route.ServiceName != "paperclip" || route.Port != 3100 || route.Ready ||
		route.NotReadyReason != "starting" || route.ClientID != "ccid" || route.IssuerURL != "https://idp.example.com" {
		t.Fatalf("route: %+v", route)
	}

	code, sid := sha256.Sum256([]byte("code")), sha256.Sum256([]byte("session"))
	if err := e.HandoffCreate(ctx, code, tm.ID, route.Hostname, "https://idp.example.com", "sub-1", idp, []string{"eng"}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := e.HandoffConsume(ctx, code, "other.collab.example.com", sid); err != nil || ok {
		t.Fatalf("a code is bound to its host: %v %v", ok, err)
	}
	team, exp, ok, err := e.HandoffConsume(ctx, code, route.Hostname, sid)
	if err != nil || !ok || team != tm.ID || !exp.After(time.Now().Add(30*time.Minute)) {
		t.Fatalf("consume: %v %v %v %v", team, exp, ok, err)
	}
	if _, _, ok, _ := e.HandoffConsume(ctx, code, route.Hostname, sid); ok {
		t.Fatal("a code works once")
	}
	s, ok, err := e.Get(ctx, sid)
	if err != nil || !ok || s.TeamID != tm.ID || s.Subject != "sub-1" || len(s.Groups) != 1 || s.ProviderID != idp {
		t.Fatalf("session: %+v %v %v", s, ok, err)
	}
	if err := e.Revoke(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Get(ctx, sid); ok {
		t.Fatal("a revoked session is gone")
	}
	if err := e.Revoke(ctx, sid); err != nil {
		t.Fatal("revoke is idempotent")
	}
	if err := e.HandoffCreate(ctx, code, pgtest.NewID("team"), route.Hostname, "i", "s", idp, nil); err == nil {
		t.Error("no handoff for a team without collaboration")
	}

	// access counts and the secret alert
	exec(`SELECT afs.ensure_collaboration_access_partition(now()::date)`)
	now := collab.BucketStart(time.Now())
	rows := []collab.Row{{Key: collab.AccessKey{Bucket: now, TeamID: tm.ID, Surface: collab.SurfaceWebUI, Kind: collab.KindUser, PrincipalID: "sub-1", Outcome: "allowed"},
		Access: collab.Access{Requests: 3, Bytes: 10}}}
	if err := e.Record(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if err := e.Record(ctx, rows); err != nil {
		t.Fatal(err)
	}
	var n int
	var bytes int64
	if err := admin.QueryRow(ctx, `SELECT sum(requests), sum(bytes) FROM afs.collaboration_access WHERE team_id = $1`, tm.ID).Scan(&n, &bytes); err != nil || n != 6 || bytes != 20 {
		t.Fatalf("counts add up: %d %d %v", n, bytes, err)
	}
	if err := e.Record(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Record(ctx, make([]collab.Row, collab.MaxFlushRows+1)); err == nil {
		t.Error("too many rows")
	}
	bad := rows
	bad[0].Key.Bucket = now.Add(-48 * time.Hour)
	if err := e.Record(ctx, bad); err == nil {
		t.Error("a bucket outside the window is refused")
	}
	if err := e.SecretDetected(ctx, tm.ID, collab.SurfaceRESTAPI, collab.KindAPIKey, "key_1", "aws_access_key", "blocked", 2); err != nil {
		t.Fatal(err)
	}
	if err := e.SecretDetected(ctx, tm.ID, collab.SurfaceRESTAPI, collab.KindAnonymous, "x", "aws_access_key", "blocked", 2); err == nil {
		t.Error("anonymous must carry no id")
	}
	var ev int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.outbox_events WHERE team_id = $1 AND event_type = 'collaboration.secret_detected'`, tm.ID).Scan(&ev); err != nil || ev != 1 {
		t.Fatalf("event: %d %v", ev, err)
	}
}
