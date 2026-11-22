package matrix

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M3-G (notification channels). Contract §7 has no channel event type (CD-7), so the
// create, update and delete operations are exempt from the outbox check; TestNotificationChannel
// writes notification.test_requested. All of them write an audit record.

func init() {
	// channel makes an email channel of team A and returns its id and version.
	channel := func(t *testing.T, w *World) map[string]string {
		id := pgtest.NewID("nch")
		w.exec(t, `INSERT INTO afs.notification_channels (id, team_id, name, kind, config) VALUES ($1, $2, $3, 'email', '{"to":["ops@example.com"]}')`,
			id, w.TeamA.ID, "mx-"+id)
		return map[string]string{"id": id, "ver": ver(t, w, `SELECT version FROM afs.notification_channels WHERE id = $1`, id)}
	}
	// svcChannel makes a service-level email channel.
	svcChannel := func(t *testing.T, w *World) map[string]string {
		id := pgtest.NewID("nch")
		w.exec(t, `INSERT INTO afs.notification_channels (id, team_id, name, kind, config) VALUES ($1, NULL, $2, 'email', '{"to":["ops@example.com"]}')`,
			id, "mx-"+id)
		return map[string]string{"id": id, "ver": ver(t, w, `SELECT version FROM afs.notification_channels WHERE id = $1`, id)}
	}
	unq := func(*testing.T, *World) map[string]string { return map[string]string{"n": unique()} }
	body := `{"name":"mx-{n}","kind":"email","config":{"to":["ops@example.com"]},"eventTypes":["budget.*"],"enabled":true}`
	ifm := map[string]string{"If-Match": ifMatch}

	Register(Case{Op: "ListNotificationChannels", Path: "/v1/teams/{teamId}/notification-channels"})
	Register(Case{Op: "GetNotificationChannel", Path: "/v1/teams/{teamId}/notification-channels/{id}", Prepare: channel})
	Register(Case{Op: "CreateNotificationChannel", Path: "/v1/teams/{teamId}/notification-channels", NoOutbox: cdOutbox, Body: body, Prepare: unq})
	Register(Case{Op: "UpdateNotificationChannel", Path: "/v1/teams/{teamId}/notification-channels/{id}", NoOutbox: cdOutbox,
		Body: `{"enabled":false}`, Headers: ifm, Prepare: channel})
	Register(Case{Op: "DeleteNotificationChannel", Path: "/v1/teams/{teamId}/notification-channels/{id}", NoOutbox: cdOutbox, Headers: ifm, Prepare: channel})
	Register(Case{Op: "TestNotificationChannel", Path: "/v1/teams/{teamId}/notification-channels/{id}:test", Prepare: channel})

	Register(Case{Op: "ListServiceNotificationChannels", Path: "/v1/admin/notification-channels"})
	Register(Case{Op: "CreateServiceNotificationChannel", Path: "/v1/admin/notification-channels", NoOutbox: cdOutbox, Body: body, Prepare: unq})
	Register(Case{Op: "UpdateServiceNotificationChannel", Path: "/v1/admin/notification-channels/{id}", NoOutbox: cdOutbox,
		Body: `{"enabled":false}`, Headers: ifm, Prepare: svcChannel})
	Register(Case{Op: "DeleteServiceNotificationChannel", Path: "/v1/admin/notification-channels/{id}", NoOutbox: cdOutbox, Headers: ifm, Prepare: svcChannel})
}
