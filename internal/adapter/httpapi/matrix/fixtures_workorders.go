package matrix

import (
	"context"
	"fmt"
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

// seedWorkOrder inserts an open-CVE work order assigned to team A, with one repository team A owns, and
// returns the placeholders a case needs. The cleanup removes it before the team goes.
func (w *World) seedWorkOrder(t *testing.T) map[string]string {
	t.Helper()
	tpl, _ := w.seedSwarmTemplate(t)
	id := pgtest.NewID("wo")
	cve := fmt.Sprintf("CVE-2026-%d", 100000+nextSeq())
	w.exec(t, `INSERT INTO afs.work_orders (id, team_id, kind, external_ref, title, sla_due_at, status, severity, template_id, budget_usd, assigned_at)
VALUES ($1, $2, 'cve_remediation', $3, 'Matrix work order', now() + interval '7 days', 'assigned', 'high', $4, 100, now())`, id, w.TeamA.ID, cve, tpl)
	w.exec(t, `INSERT INTO afs.work_order_repos (work_order_id, host, repo, owning_team_id) VALUES ($1, 'github.com', 'mx/repo', $2)`, id, w.TeamA.ID)
	t.Cleanup(func() {
		_, _ = w.Admin.Exec(context.WithoutCancel(t.Context()), `DELETE FROM afs.work_orders WHERE id = $1`, id)
	})
	return map[string]string{"wid": id, "tid": tpl, "cve": cve,
		"ver":  ver(t, w, `SELECT version FROM afs.work_orders WHERE id = $1`, id),
		"rver": ver(t, w, `SELECT version FROM afs.work_order_repos WHERE work_order_id = $1`, id)}
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

	// Work orders.
	Register(Case{Op: "ListWorkOrders", Path: "/v1/work-orders"})
	Register(Case{Op: "ListTeamWorkOrders", Path: "/v1/teams/{teamId}/work-orders"})
	Register(Case{Op: "CreateWorkOrder", Path: "/v1/work-orders",
		Body: `{"cveId":"{cve}","severity":"high","title":"Matrix","slaDueAt":"2099-01-01T00:00:00Z","templateId":"{tid}","repos":[{"repo":"mx/created","owningContact":"owner@example.com"}]}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			tpl, _ := w.seedSwarmTemplate(t)
			return map[string]string{"tid": tpl, "cve": fmt.Sprintf("CVE-2026-%d", 100000+nextSeq())}
		}})
	Register(Case{Op: "GetWorkOrder", Path: "/v1/work-orders/{wid}", Prepare: func(t *testing.T, w *World) map[string]string { return w.seedWorkOrder(t) }})
	Register(Case{Op: "ListWorkOrderRepos", Path: "/v1/work-orders/{wid}/repos", Prepare: func(t *testing.T, w *World) map[string]string { return w.seedWorkOrder(t) }})
	Register(Case{Op: "UpdateWorkOrder", Path: "/v1/work-orders/{wid}", Headers: map[string]string{"If-Match": ifMatch},
		Body:    `{"title":"Renamed","addRepos":[{"repo":"mx/added","owningContact":"owner@example.com"}]}`,
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedWorkOrder(t) }})
	Register(Case{Op: "CancelWorkOrder", Path: "/v1/work-orders/{wid}:cancel", Body: `{"reason":"matrix"}`,
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedWorkOrder(t) }})
	Register(Case{Op: "UpdateWorkOrderRepo", Path: "/v1/work-orders/{wid}/repos/github.com/mx/repo", Headers: map[string]string{"If-Match": `"{rver}"`},
		Body:    `{"status":"in_progress","detail":"working"}`,
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedWorkOrder(t) }})
}
