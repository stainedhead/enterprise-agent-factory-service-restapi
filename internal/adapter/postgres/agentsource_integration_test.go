package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

const agentDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type seededAgent struct {
	team           pgtest.Team
	agentID        string
	templateID     string
	secretA, extID string
}

// seedAgent inserts a team with a template-backed agent, two secret bindings (one for an external system),
// configuration values (one linked to the namespace) and a team tag.
func seedFullAgent(t *testing.T, db *pgtest.DB) seededAgent {
	t.Helper()
	ctx := t.Context()
	admin := db.Admin(t)
	s := seededAgent{team: db.SeedTeam(t), agentID: pgtest.NewID("agt"), templateID: pgtest.NewID("tpl"), secretA: pgtest.NewID("sec"), extID: pgtest.NewID("ext")}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", strings.Fields(sql)[2], err)
		}
	}
	exec(`INSERT INTO afs.models (id, provider, display_name) VALUES ('claude-sonnet', 'Anthropic', 'Claude Sonnet') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'claude-sonnet')`, s.team.ID)
	exec(`INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest) VALUES ($1, $2, 'hermes', ARRAY[$3]::text[], $3)`, s.templateID, "tpl-"+strings.ToLower(s.templateID[4:14]), agentDigest)
	exec(`INSERT INTO afs.agents (id, team_id, name, template, template_id, image, model_id, resources, storage_size_gib, iam_identity)
	      VALUES ($1, $2, 'reviewer', 'hermes', $3, 'ghcr.io/x/hermes@`+agentDigest+`', 'claude-sonnet', '{"cpu":"500m","memory":"1Gi"}', 30, 'dedicated')`, s.agentID, s.team.ID, s.templateID)
	exec(`INSERT INTO afs.external_systems (id, slug, name, category, endpoints) VALUES ($1, $2, 'Okta', 'identity', ARRAY['example.okta.com'])`, s.extID, "okta-"+strings.ToLower(s.extID[4:12]))
	const arn = `'arn:aws:secretsmanager:us-east-1:111122223333:secret:' || afs.secret_path($2, NULL, $3) || '-AbCdEf'`
	exec(`INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin, current_version_id) VALUES ($1, $2, $3, `+arn+`, 'managed', 'v1')`, s.secretA, s.team.ID, "gh")
	secretB := pgtest.NewID("sec")
	exec(`INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin, external_system_id) VALUES ($1, $2, $3, `+arn+`, 'managed', $4)`, secretB, s.team.ID, "okta", s.extID)
	exec(`INSERT INTO afs.agent_secret_bindings (team_id, agent_id, secret_id, delivery, target) VALUES ($1, $2, $3, 'env', 'GH_TOKEN'), ($1, $2, $4, 'file', '/etc/okta/token')`, s.team.ID, s.agentID, s.secretA, secretB)
	exec(`INSERT INTO afs.namespace_config_values (id, team_id, name, config_value) VALUES ($1, $2, 'REGION', 'eu-west-1')`, pgtest.NewID("ncv"), s.team.ID)
	exec(`INSERT INTO afs.agent_config_values (id, team_id, agent_id, name, source, config_value) VALUES
	      ($1, $3, $4, 'LOG_LEVEL', 'literal', 'info'), ($2, $3, $4, 'REGION', 'namespace', NULL)`, pgtest.NewID("acv"), pgtest.NewID("acv"), s.team.ID, s.agentID)
	return s
}

// M2-F: the reconciler reads the agent with its bindings, resolved configuration, tags, volume settings,
// identity and team context.
func TestAgentSourceLoadAgent(t *testing.T) {
	db := pgtest.Get(t)
	s := seedFullAgent(t, db)
	src := postgres.NewAgentSource(reconcilerPool(t, db))

	d, err := src.LoadAgent(t.Context(), s.agentID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "reviewer" || d.Template != "hermes" || d.State != "enabled" || d.IAMIdentity != "dedicated" || d.ModelID != "claude-sonnet" {
		t.Errorf("agent = %+v", d)
	}
	if d.Resources.CPU != "500m" || d.Resources.Memory != "1Gi" || d.Volume.SizeGiB != 30 || d.Volume.StorageClass != "gp3" || d.Volume.OnDelete != "snapshot_then_delete" || d.Volume.SnapshotSchedule != "daily" || d.Volume.SnapshotRetain != 7 {
		t.Errorf("resources/volume = %+v %+v", d.Resources, d.Volume)
	}
	if d.Team.ID != s.team.ID || d.Team.Namespace != "afs-t-"+s.team.Slug || d.Team.State != "active" || d.Team.IsolationTier != "shared" || d.Team.WorkOrderAssigned {
		t.Errorf("team = %+v", d.Team)
	}
	if len(d.Bindings) != 2 {
		t.Fatalf("bindings = %+v", d.Bindings)
	}
	env, file := d.Bindings[0], d.Bindings[1]
	if env.Delivery != "env" || env.Target != "GH_TOKEN" || env.VersionID != "v1" || !strings.HasPrefix(env.SecretARN, "arn:aws:secretsmanager:") || !env.Delivered() {
		t.Errorf("env binding = %+v", env)
	}
	if file.Delivery != "file" || file.ExternalSystemID != s.extID || file.SystemOff || !file.Delivered() {
		t.Errorf("file binding = %+v", file)
	}
	if len(d.Config) != 2 || d.Config[0] != (agent.ConfigValue{Name: "LOG_LEVEL", Value: "info"}) || d.Config[1] != (agent.ConfigValue{Name: "REGION", Value: "eu-west-1"}) {
		t.Errorf("config = %+v", d.Config)
	}
	if d.Tags["afs-agent-id"] != s.agentID || d.Tags["afs-team-id"] != s.team.ID {
		t.Errorf("tags = %v", d.Tags)
	}
	if d.TemplateRetired || d.RestartNonce != "" {
		t.Errorf("template retired %v nonce %q", d.TemplateRetired, d.RestartNonce)
	}

	if _, err := src.LoadAgent(t.Context(), pgtest.NewID("agt")); !isNotFound(err) {
		t.Errorf("missing agent: %v", err)
	}
	list, err := src.ListTeamAgents(t.Context(), s.team.ID)
	if err != nil || len(list) != 1 || list[0].ID != s.agentID || list[0].Namespace != d.Team.Namespace {
		t.Errorf("ListTeamAgents = %+v, %v", list, err)
	}
}

func isNotFound(err error) bool { return domain.KindOf(err) == domain.KindNotFound }

// M2-F, EXT-7: switching the external system off for the team (or the agent) marks the binding.
func TestAgentSourceSwitchedOffSystemAndTemplateRetired(t *testing.T) {
	db := pgtest.Get(t)
	s := seedFullAgent(t, db)
	admin := db.Admin(t)
	src := postgres.NewAgentSource(reconcilerPool(t, db))
	load := func() agent.Desired {
		t.Helper()
		d, err := src.LoadAgent(t.Context(), s.agentID)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.external_system_optouts (id, team_id, agent_id, system_id) VALUES ($1, $2, $3, $4)`, pgtest.NewID("xso"), s.team.ID, s.agentID, s.extID); err != nil {
		t.Fatal(err)
	}
	if b := load().Bindings[1]; !b.SystemOff || b.Delivered() {
		t.Errorf("binding of a switched-off system = %+v", b)
	}
	if _, err := admin.Exec(t.Context(), `DELETE FROM afs.external_system_optouts WHERE agent_id = $1`, s.agentID); err != nil {
		t.Fatal(err)
	}
	if b := load().Bindings[1]; b.SystemOff {
		t.Errorf("after switching on: %+v", b)
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.external_systems SET enabled = false WHERE id = $1`, s.extID); err != nil {
		t.Fatal(err)
	}
	if b := load().Bindings[1]; !b.SystemOff {
		t.Error("a service-level switch-off did not reach the binding")
	}

	// RA-US-4.5, M2-F6: disabling the template or removing the digest flags the agent; nothing else changes.
	if _, err := admin.Exec(t.Context(), `UPDATE afs.agent_templates SET enabled = false WHERE id = $1`, s.templateID); err != nil {
		t.Fatal(err)
	}
	d := load()
	if !d.TemplateRetired || !strings.Contains(d.TemplateReason, "disabled") || d.State != "enabled" {
		t.Errorf("disabled template: %+v %q", d.TemplateRetired, d.TemplateReason)
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.agent_templates SET enabled = true WHERE id = $1`, s.templateID); err != nil {
		t.Fatal(err)
	}
	if load().TemplateRetired {
		t.Error("re-enabling did not clear TemplateRetired")
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.agent_templates SET allowed_image_digests = ARRAY['sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'], current_image_digest = 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' WHERE id = $1`, s.templateID); err != nil {
		t.Fatal(err)
	}
	if d := load(); !d.TemplateRetired || !strings.Contains(d.TemplateReason, "digest") {
		t.Errorf("removed digest: %v %q", d.TemplateRetired, d.TemplateReason)
	}
}

// The restart nonce follows the agent's restarts and nobody else's; a work order assigns the afs-api token.
func TestAgentSourceRestartNonceAndWorkOrder(t *testing.T) {
	db := pgtest.Get(t)
	s := seedFullAgent(t, db)
	other := seedFullAgent(t, db)
	admin := db.Admin(t)
	src := postgres.NewAgentSource(reconcilerPool(t, db))
	// The outbox is shared by the package's tests; leave it as found.
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DELETE FROM afs.outbox_events WHERE event_type = 'agent.restarted'`); err != nil {
			t.Error(err)
		}
	})
	restart := func(a seededAgent) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), `INSERT INTO afs.outbox_events (event_type, aggregate_kind, aggregate_id, team_id, payload) VALUES ('agent.restarted', 'agent', $1, $2, '{}')`, a.agentID, a.team.ID); err != nil {
			t.Fatal(err)
		}
	}
	nonce := func(a seededAgent) string {
		d, err := src.LoadAgent(t.Context(), a.agentID)
		if err != nil {
			t.Fatal(err)
		}
		return d.RestartNonce
	}
	if nonce(s) != "" {
		t.Fatal("nonce before any restart")
	}
	restart(s)
	n1 := nonce(s)
	if n1 == "" || nonce(other) != "" {
		t.Errorf("nonce = %q, other agent's = %q", n1, nonce(other))
	}
	restart(s)
	if nonce(s) == n1 {
		t.Error("a second restart did not change the nonce")
	}
	d, _ := src.LoadAgent(t.Context(), s.agentID)
	if d.Team.WorkOrderAssigned {
		t.Error("work order flagged without one")
	}
}

func TestParseAgentResources(t *testing.T) {
	tests := []struct {
		in      string
		want    agent.Resources
		wantErr bool
	}{
		{``, agent.Resources{}, false},
		{`{}`, agent.Resources{}, false},
		{`{"cpu":"500m","memory":"1Gi"}`, agent.Resources{CPU: "500m", Memory: "1Gi"}, false},
		{`{"cpu":2,"memory":null}`, agent.Resources{CPU: "2"}, false},
		{`{"cpu":true}`, agent.Resources{}, true},
		{`[1]`, agent.Resources{}, true},
	}
	for _, tt := range tests {
		got, err := postgres.ParseAgentResources([]byte(tt.in))
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseAgentResources(%q) = %+v, %v", tt.in, got, err)
		}
	}
}

// MEG-RC-3: on a per-namespace team the reconciler reads the agent's effective routes and its route default as
// <route>/<model>; a route default that leaves the route set gets the route admission reason.
func TestAgentSourceRoutesAgainstTheSchema(t *testing.T) {
	db := pgtest.Get(t)
	s := seedFullAgent(t, db)
	admin := db.Admin(t)
	ctx := t.Context()
	slug, ep := mepSlug("ar"), pgtest.NewID("mep")
	mustExec(t, admin, `UPDATE afs.teams SET gateway_deployment = 'per_namespace' WHERE id = $1`, s.team.ID)
	mustExec(t, admin, `INSERT INTO afs.model_endpoints (id, slug, display_name, location, base_url, auth_kind, pricing_mode, enabled, protocols)
VALUES ($1, $2, 'Routes', 'external', 'https://llm.example.com/v1', 'none', 'free', true, '{openai_chat,openai_responses}')`, ep, slug)
	mustExec(t, admin, `INSERT INTO afs.endpoint_models (endpoint_id, model_name, max_output_tokens) VALUES ($1, 'm1', 4096)`, ep)
	mustExec(t, admin, `INSERT INTO afs.team_model_endpoints (team_id, endpoint_id) VALUES ($1, $2)`, s.team.ID, ep)
	mustExec(t, admin, `UPDATE afs.agents SET default_model_id = $2 WHERE id = $1`, s.agentID, slug+"/m1")

	src := postgres.NewAgentSource(reconcilerPool(t, db))
	d, err := src.LoadAgent(ctx, s.agentID)
	if err != nil {
		t.Fatal(err)
	}
	r := d.Routes
	if d.Team.GatewayDeployment != agent.GatewayPerNamespace || d.ModelID != slug+"/m1" || r.DefaultRoute != slug || r.DefaultModel != "m1" ||
		len(r.Routes) != 1 || r.Routes[0].Models[0].MaxOutputTokens != 4096 || len(r.Routes[0].Protocols) != 2 || d.ModelReason != "" {
		t.Errorf("routes = %+v model %s reason %q", r, d.ModelID, d.ModelReason)
	}
	// The endpoint is switched off: the default is refused with the route admission reason.
	mustExec(t, admin, `UPDATE afs.model_endpoints SET enabled = false WHERE id = $1`, ep)
	d, err = src.LoadAgent(ctx, s.agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Routes.Routes) != 0 || d.ModelReason != "route_not_enabled_for_service" {
		t.Errorf("after disable: %+v %q", d.Routes, d.ModelReason)
	}
}
