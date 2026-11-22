package matrix

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M6-A: security operators, service-level API keys and the team directory.
func init() {
	Register(Case{Op: "ListTeamDirectory", Path: "/v1/team-directory"})

	Register(Case{Op: "ListSecurityOperators", Path: "/v1/admin/security-operators"})
	Register(Case{Op: "AddSecurityOperator", Path: "/v1/admin/security-operators", NoOutbox: cdOutbox,
		Body:    `{"subjectType":"group","groupName":"mx-secop-{n}"}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": unique()} }})
	Register(Case{Op: "RemoveSecurityOperator", Path: "/v1/admin/security-operators/{bindingId}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string {
			id := pgtest.NewID("rb")
			w.exec(t, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name) VALUES ($1, 'security_operator', NULL, 'group', $2)`,
				id, "mx-secop-del-"+id)
			return map[string]string{"bindingId": id, "ver": ver(t, w, `SELECT version FROM afs.role_bindings WHERE id = $1`, id)}
		}})

	Register(Case{Op: "ListServiceApiKeys", Path: "/v1/admin/api-keys"})
	Register(Case{Op: "CreateServiceApiKey", Path: "/v1/admin/api-keys", NoOutbox: cdOutbox,
		Body: `{"name":"mx-svc-{n}","role":"security_operator","expiresAt":"{exp}"}`,
		Prepare: func(*testing.T, *World) map[string]string {
			return map[string]string{"n": unique(), "exp": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)}
		}})
	// CQ-2: no If-Match, an API key has no version.
	Register(Case{Op: "DeleteServiceApiKey", Path: "/v1/admin/api-keys/{keyId}", NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string {
			id := pgtest.NewID("key")
			sum := sha256.Sum256([]byte(id))
			w.exec(t, `INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES ($1, NULL, $2, 'security_operator', 'afsk_mx', $3, now() + interval '60 days')`,
				id, "mx-svc-del-"+id, sum[:])
			return map[string]string{"keyId": id}
		}})
}
