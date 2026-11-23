package matrix

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M1-B (teams), M1-C (members and API keys) and M1-E (service administration I). Each case
// creates the resource it names in Prepare, because the harness repeats a request as every caller and
// a destructive operation must find its target again.

const (
	cdOutbox = "CD-7: this mutation has no event type in the API contract"
	ifMatch  = `"{ver}"`
)

func ver(t *testing.T, w *World, q string, args ...any) string {
	t.Helper()
	var v int64
	if err := w.Admin.QueryRow(t.Context(), q, args...).Scan(&v); err != nil {
		t.Fatalf("matrix prepare: %v\n%s", err, q)
	}
	return strconv.FormatInt(v, 10)
}

func (w *World) teamVersion(t *testing.T) string {
	return ver(t, w, `SELECT version FROM afs.teams WHERE id = $1`, w.TeamA.ID)
}

func unique() string {
	return strconv.Itoa(nextSeq()) + "x" + strconv.FormatInt(time.Now().UnixNano()%1e6, 10)
}

func init() {
	// Teams.
	for _, c := range []struct{ op, path string }{
		{"GetTeam", "/v1/teams/{teamId}"}, {"GetTeamStatus", "/v1/teams/{teamId}/status"}, {"ListTeamModels", "/v1/teams/{teamId}/models"},
		{"GetTeamTags", "/v1/teams/{teamId}/tags"}, {"ListMembers", "/v1/teams/{teamId}/members"}, {"ListApiKeys", "/v1/teams/{teamId}/api-keys"},
		{"ListAuditEvents", "/v1/teams/{teamId}/audit-events"}, {"ListRevisions", "/v1/teams/{teamId}/revisions"},
	} {
		Register(Case{Op: c.op, Path: c.path})
	}
	Register(Case{Op: "ListTeams", Path: "/v1/teams"})
	Register(Case{Op: "GetRevision", Path: "/v1/teams/{teamId}/revisions/{rev}",
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.exec(t, `UPDATE afs.teams SET description = 'matrix ' || clock_timestamp() WHERE id = $1`, w.TeamA.ID)
			return map[string]string{"rev": ver(t, w, `SELECT max(id) FROM afs.config_revisions WHERE team_id = $1`, w.TeamA.ID)}
		}})
	Register(Case{Op: "CreateTeam", Path: "/v1/teams", NoAudit: "CreateTeam also audits the orchestration agent and its boot secrets, in the same transaction (ORC-2)",
		Body:    `{"slug":"mx{slug}","displayName":"Matrix","kind":"persistent","costCenter":"CC1","ownerEmail":"o@example.com","maxBudgetUsd":"1000","firstAdmin":{"subjectType":"group","groupName":"mx-first-{slug}"},"tags":[]}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"slug": fmt5(nextSeq())} }})
	Register(Case{Op: "UpdateTeam", Path: "/v1/teams/{teamId}", Body: `{"displayName":"Matrix"}`, Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"ver": w.teamVersion(t)} }})
	Register(Case{Op: "SuspendTeam", Path: "/v1/teams/{teamId}:suspend"})
	Register(Case{Op: "ResumeTeam", Path: "/v1/teams/{teamId}:resume", Suspended: true,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.exec(t, `UPDATE afs.teams SET state = 'suspended' WHERE id = $1`, w.TeamA.ID)
			return nil
		}})
	Register(Case{Op: "DeleteTeam", Path: "/v1/teams/{teamId}", Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"ver": w.teamVersion(t)} }})
	Register(Case{Op: "PutTeamModels", Path: "/v1/teams/{teamId}/models", Body: `{"access":"all"}`})
	Register(Case{Op: "PutTeamTags", Path: "/v1/teams/{teamId}/tags", Body: `{"tags":[]}`})

	// Members.
	Register(Case{Op: "AddMember", Path: "/v1/teams/{teamId}/members", NoOutbox: cdOutbox,
		Body:    `{"role":"team_viewer","subjectType":"group","groupName":"mx-added-{n}"}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": unique()} }})
	member := func(t *testing.T, w *World) map[string]string {
		id := pgtest.NewID("rb")
		w.exec(t, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name) VALUES ($1, 'team_viewer', $2, 'group', $3)`,
			id, w.TeamA.ID, "mx-member-"+id)
		return map[string]string{"bindingId": id, "ver": ver(t, w, `SELECT version FROM afs.role_bindings WHERE id = $1`, id)}
	}
	Register(Case{Op: "UpdateMember", Path: "/v1/teams/{teamId}/members/{bindingId}", Body: `{"role":"team_admin"}`, NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: member})
	Register(Case{Op: "RemoveMember", Path: "/v1/teams/{teamId}/members/{bindingId}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: member})

	// API keys.
	Register(Case{Op: "CreateApiKey", Path: "/v1/teams/{teamId}/api-keys", NoOutbox: cdOutbox,
		Body: `{"name":"mx-{n}","role":"team_viewer","expiresAt":"{exp}"}`,
		Prepare: func(*testing.T, *World) map[string]string {
			return map[string]string{"n": unique(), "exp": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)}
		}})
	// CQ-2: no If-Match, an API key has no version.
	Register(Case{Op: "DeleteApiKey", Path: "/v1/teams/{teamId}/api-keys/{keyId}", NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string {
			id := pgtest.NewID("key")
			sum := sha256.Sum256([]byte(id))
			w.exec(t, `INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES ($1, $2, $3, 'team_viewer', 'afsk_mx', $4, now() + interval '60 days')`,
				id, w.TeamA.ID, "mx-del-"+id, sum[:])
			return map[string]string{"keyId": id}
		}})

	// Service administration I.
	Register(Case{Op: "GetSettings", Path: "/v1/admin/settings"})
	Register(Case{Op: "PutSetting", Path: "/v1/admin/settings/teams.ephemeral_max_days", Body: `{"value":90}`, NoOutbox: cdOutbox})
	Register(Case{Op: "ListIdentityProviders", Path: "/v1/admin/identity-providers"})
	Register(Case{Op: "GetIdentityProvider", Path: "/v1/admin/identity-providers/{id}",
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"id": w.ProviderID} }})
	Register(Case{Op: "CreateIdentityProvider", Path: "/v1/admin/identity-providers", NoOutbox: cdOutbox,
		Body:    `{"kind":"okta","name":"mx-{n}","issuerUrl":"https://mx-{n}.example.com","consoleClientId":"c-{n}","cliClientId":"cli-{n}","enabled":false}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": unique()} }})
	idp := func(t *testing.T, w *World) map[string]string {
		n, id := unique(), pgtest.NewID("idp")
		w.exec(t, `INSERT INTO afs.identity_providers (id, kind, name, issuer_url, client_id, enabled) VALUES ($1, 'okta', $2, $3, $4, false)`,
			id, "mx-"+n, "https://mx-"+n+".example.com", "c-"+n)
		return map[string]string{"id": id, "ver": ver(t, w, `SELECT version FROM afs.identity_providers WHERE id = $1`, id)}
	}
	Register(Case{Op: "UpdateIdentityProvider", Path: "/v1/admin/identity-providers/{id}", Body: `{"name":"mx-renamed-{ver}"}`, NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: idp})
	Register(Case{Op: "DeleteIdentityProvider", Path: "/v1/admin/identity-providers/{id}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: idp})
	Register(Case{Op: "ListServiceTags", Path: "/v1/admin/service-tags"})
	Register(Case{Op: "PutServiceTag", Path: "/v1/admin/service-tags/mx-put", Body: `{"mode":"fixed","value":"matrix"}`, NoOutbox: cdOutbox})
	Register(Case{Op: "DeleteServiceTag", Path: "/v1/admin/service-tags/mx-del", NoOutbox: cdOutbox, Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.exec(t, `INSERT INTO afs.service_tags (key, mode, value) VALUES ('mx-del', 'fixed', 'x') ON CONFLICT (key) DO NOTHING`)
			return map[string]string{"ver": ver(t, w, `SELECT version FROM afs.service_tags WHERE key = 'mx-del'`)}
		}})
	Register(Case{Op: "ListAdminAuditEvents", Path: "/v1/admin/audit-events"})
}

func fmt5(n int) string { return fmt.Sprintf("%05d", n) }
