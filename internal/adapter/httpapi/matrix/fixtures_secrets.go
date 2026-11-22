package matrix

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M2-C (secrets). The value in every request body is a fixture, never a real credential.

// seedSecret inserts a team secret of team A with an ARN that matches afs.secret_path and returns
// the placeholders a case needs.
func (w *World) seedSecret(t *testing.T) map[string]string {
	t.Helper()
	var ns string
	if err := w.Admin.QueryRow(t.Context(), `SELECT namespace FROM afs.teams WHERE id = $1`, w.TeamA.ID).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	id, name := pgtest.NewID("sec"), "mx-"+unique()
	arn := "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/" + ns + "/_shared/" + name + "-AbCdEf"
	w.exec(t, `INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin, current_version_id) VALUES ($1, $2, $3, $4, 'managed', 'v1')`,
		id, w.TeamA.ID, name, arn)
	return map[string]string{"secretId": id, "ver": ver(t, w, `SELECT version FROM afs.secrets WHERE id = $1`, id)}
}

func init() {
	const secretValue = `"value":"matrix-fixture-value"`
	Register(Case{Op: "ListSecrets", Path: "/v1/teams/{teamId}/secrets"})
	Register(Case{Op: "GetSecret", Path: "/v1/teams/{teamId}/secrets/{secretId}",
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedSecret(t) }})
	Register(Case{Op: "CreateSecret", Path: "/v1/teams/{teamId}/secrets",
		Body:    `{"scope":"team","name":"mx-{n}",` + secretValue + `}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": unique()} }})
	Register(Case{Op: "UpdateSecret", Path: "/v1/teams/{teamId}/secrets/{secretId}", Body: `{"description":"matrix"}`,
		NoOutbox: cdOutbox, Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedSecret(t) }})
	Register(Case{Op: "DeleteSecret", Path: "/v1/teams/{teamId}/secrets/{secretId}", Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedSecret(t) }})
	Register(Case{Op: "PutSecretValue", Path: "/v1/teams/{teamId}/secrets/{secretId}/value", Body: `{` + secretValue + `}`,
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedSecret(t) }})
	Register(Case{Op: "RotateSecret", Path: "/v1/teams/{teamId}/secrets/{secretId}:rotate", Body: `{` + secretValue + `}`,
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedSecret(t) }})
}
