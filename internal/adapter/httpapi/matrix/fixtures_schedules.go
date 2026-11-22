package matrix

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M4-E2 (report schedules). Contract §7 has no schedule event, so the mutations are exempt
// from the outbox check (CD-7); each writes an audit record. The channel in every case is a
// notification channel of team A, so any caller that may write the schedules of team A can use it.

func init() {
	// ownChannel makes an email channel of team A.
	ownChannel := func(t *testing.T, w *World) string {
		id := pgtest.NewID("nch")
		w.exec(t, `INSERT INTO afs.notification_channels (id, team_id, name, kind, config) VALUES ($1, $2, $3, 'email', '{"to":["ops@example.com"]}')`,
			id, w.TeamA.ID, "mx-"+id)
		return id
	}
	// schedule makes a weekly usage schedule of team A on its own channel.
	schedule := func(t *testing.T, w *World) map[string]string {
		ch := ownChannel(t, w)
		id := pgtest.NewID("rsc")
		w.exec(t, `INSERT INTO afs.report_schedules (id, team_id, name, report, query, cadence, channel_id, next_run_at)
VALUES ($1, $2, $3, 'usage', '{"range":"7d"}', 'weekly', $4, now() + interval '1 day')`, id, w.TeamA.ID, "mx-"+id, ch)
		return map[string]string{"id": id, "ch": ch, "ver": ver(t, w, `SELECT version FROM afs.report_schedules WHERE id = $1`, id)}
	}
	create := func(t *testing.T, w *World) map[string]string {
		return map[string]string{"ch": ownChannel(t, w), "n": unique()}
	}
	ifm := map[string]string{"If-Match": ifMatch}

	Register(Case{Op: "ListReportSchedules", Path: "/v1/teams/{teamId}/report-schedules"})
	Register(Case{Op: "CreateReportSchedule", Path: "/v1/teams/{teamId}/report-schedules", NoOutbox: cdOutbox, Prepare: create,
		Body: `{"name":"mx-{n}","report":"usage","cadence":"weekly","channelId":"{ch}","query":{"range":"7d","groupBy":["agent"]}}`})
	Register(Case{Op: "UpdateReportSchedule", Path: "/v1/teams/{teamId}/report-schedules/{id}", NoOutbox: cdOutbox, Prepare: schedule,
		Headers: ifm, Body: `{"enabled":false}`})
	Register(Case{Op: "DeleteReportSchedule", Path: "/v1/teams/{teamId}/report-schedules/{id}", NoOutbox: cdOutbox, Prepare: schedule, Headers: ifm})
}
