package matrix

import (
	"strconv"
	"testing"
)

// Case of M4-G (restore). team_admin and service_admin may restore; a viewer is refused. Restoring the
// newest revision changes nothing but still writes the restore's audit record and team.updated.

func init() {
	Register(Case{Op: "RestoreRevision", Path: "/v1/teams/{teamId}/revisions/{rev}:restore", Body: `{"dryRun":false}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			return map[string]string{"rev": strconv.Itoa(w.count(t, `SELECT max(id) FROM afs.config_revisions WHERE team_id = $1`, w.TeamA.ID))}
		}})
}
