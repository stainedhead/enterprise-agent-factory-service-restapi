package matrix

import (
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M6-B (swarm templates) and M6-C (work orders). Work orders are visible to team A only when
// they are assigned to it, so the seed assigns each one to team A (status assigned, team_id A).

const mxSpec = `{"stages":[],"pools":[{"name":"fixer","template":"coder","image":"registry.example.com/a@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","modelId":"mx-model"}]}`

// seedSwarmTemplate inserts an enabled template with a unique name and returns its id and version.
func (w *World) seedSwarmTemplate(t *testing.T) (id, version string) {
	t.Helper()
	id = pgtest.NewID("swt")
	w.exec(t, `INSERT INTO afs.swarm_templates (id, name, kind, spec, budget_usd, ttl_hours) VALUES ($1, $2, 'cve_remediation', $3::jsonb, 100, 24)`,
		id, "mx-swt-"+strings.ToLower(unique()), mxSpec)
	return id, ver(t, w, `SELECT version FROM afs.swarm_templates WHERE id = $1`, id)
}

func init() {
	Register(Case{Op: "ListSwarmTemplates", Path: "/v1/admin/swarm-templates"})
	Register(Case{Op: "CreateSwarmTemplate", Path: "/v1/admin/swarm-templates", NoOutbox: cdOutbox,
		Body:    `{"name":"mx-new-{n}","kind":"cve_remediation","spec":` + mxSpec + `,"budgetUsd":"100.00","ttlHours":24,"teardownRule":"at_work_complete"}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": strings.ToLower(unique())} }})
	tpl := func(t *testing.T, w *World) map[string]string {
		id, v := w.seedSwarmTemplate(t)
		return map[string]string{"tid": id, "ver": v}
	}
	Register(Case{Op: "GetSwarmTemplate", Path: "/v1/admin/swarm-templates/{tid}", Prepare: tpl})
	Register(Case{Op: "UpdateSwarmTemplate", Path: "/v1/admin/swarm-templates/{tid}", Body: `{"description":"changed"}`, NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: tpl})
	Register(Case{Op: "DeleteSwarmTemplate", Path: "/v1/admin/swarm-templates/{tid}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: tpl})
	Register(Case{Op: "EnableSwarmTemplate", Path: "/v1/admin/swarm-templates/{tid}:enable", NoOutbox: cdOutbox, Prepare: tpl})
	Register(Case{Op: "DisableSwarmTemplate", Path: "/v1/admin/swarm-templates/{tid}:disable", NoOutbox: cdOutbox, Prepare: tpl})
}
