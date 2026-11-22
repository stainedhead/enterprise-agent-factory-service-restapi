package matrix

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M2-I1 (log destinations). Contract §7 has no log destination event type (CD-7), so the
// mutations are exempt from the outbox check; they still write an audit record.

func init() {
	// logDest makes a CloudWatch destination of team A and returns its id and version.
	logDest := func(t *testing.T, w *World) map[string]string {
		id := pgtest.NewID("log")
		w.exec(t, `INSERT INTO afs.log_destinations (id, team_id, name, kind, config) VALUES ($1, $2, $3, 'cloudwatch', '{"logGroup":"/afs/mx","region":"us-east-1"}')`,
			id, w.TeamA.ID, "mx-"+id)
		return map[string]string{"id": id, "ver": ver(t, w, `SELECT version FROM afs.log_destinations WHERE id = $1`, id)}
	}
	Register(Case{Op: "ListLogDestinations", Path: "/v1/teams/{teamId}/log-destinations"})
	Register(Case{Op: "GetLogDestination", Path: "/v1/teams/{teamId}/log-destinations/{id}", Prepare: logDest})
	Register(Case{Op: "CreateLogDestination", Path: "/v1/teams/{teamId}/log-destinations", NoOutbox: cdOutbox,
		Body:    `{"name":"mx-{n}","kind":"cloudwatch","config":{"logGroup":"/afs/mx","region":"us-east-1"},"enabled":true}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": unique()} }})
	Register(Case{Op: "UpdateLogDestination", Path: "/v1/teams/{teamId}/log-destinations/{id}", NoOutbox: cdOutbox,
		Body: `{"enabled":false}`, Headers: map[string]string{"If-Match": ifMatch}, Prepare: logDest})
	Register(Case{Op: "DeleteLogDestination", Path: "/v1/teams/{teamId}/log-destinations/{id}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: logDest})
}
