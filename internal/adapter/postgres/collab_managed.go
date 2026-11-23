package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	rcollab "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

var (
	_ rcollab.ManagedSource = (*CollabReconcile)(nil)
	_ rcollab.BuzzSource    = (*CollabReconcile)(nil)
	_ rcollab.PeopleSource  = (*CollabReconcile)(nil)
	_ rcollab.Admins        = (*CollabReconcile)(nil)
)

// managedTeamsSQL lists the collaboration agents in managed mode whose board key is recorded: the teams the
// registrar may call. The pod state is the agent controller's observation.
const managedTeamsSQL = `SELECT c.team_id, c.agent_id, coalesce(ars.phase = 'Ready', false)
FROM afs.team_collaboration c
JOIN afs.teams t ON t.id = c.team_id AND t.deleted_at IS NULL AND t.state NOT IN ('deleting', 'deleted')
JOIN afs.agents a ON a.team_id = c.team_id AND a.id = c.agent_id AND a.deleted_at IS NULL
LEFT JOIN afs.resource_status ars ON ars.resource_kind = 'agent' AND ars.resource_id = c.agent_id
WHERE c.registration_mode = 'managed'
  AND EXISTS (SELECT 1 FROM afs.secrets s WHERE s.team_id = c.team_id AND s.agent_id = c.agent_id AND s.name = '` + domain.SecretPaperclipBoardAPIKey + `')
ORDER BY c.team_id`

// managedLinksSQL lists every link with what the registrar needs: the agent's name and persona, whether it is
// paused (itself or by its team), the recorded key and the path the key goes to.
const managedLinksSQL = `SELECT ac.team_id, ac.agent_id, coalesce(a.name, ''), coalesce(a.persona, ''), ac.connect,
  a.id IS NULL OR a.deleted_at IS NOT NULL,
  tc.team_id IS NULL OR ca.id IS NULL,
  coalesce(a.state = 'paused', false) OR coalesce(t.state = 'suspended', false),
  ac.link_state, coalesce(ac.paperclip_agent_id, ''), coalesce(ac.last_error, ''),
  coalesce(s.secrets_manager_arn, ''), afs.secret_path(ac.team_id, ac.agent_id, '` + domain.SecretPaperclipAgentAPIKey + `'),
  coalesce(tc.registration_mode, '')
FROM afs.agent_collaboration ac
LEFT JOIN afs.agents a ON a.team_id = ac.team_id AND a.id = ac.agent_id
LEFT JOIN afs.teams t ON t.id = ac.team_id
LEFT JOIN afs.team_collaboration tc ON tc.team_id = ac.team_id
LEFT JOIN afs.agents ca ON ca.team_id = tc.team_id AND ca.id = tc.agent_id AND ca.deleted_at IS NULL
LEFT JOIN afs.secrets s ON s.team_id = ac.team_id AND s.agent_id = ac.agent_id AND s.name = '` + domain.SecretPaperclipAgentAPIKey + `'
ORDER BY ac.team_id, ac.agent_id`

// ManagedTeams implements collab.ManagedSource. Links of teams that lost their collaboration agent come back
// as one team with Gone set per team: Paperclip went with the pod, so only the records and secrets remain.
func (c *CollabReconcile) ManagedTeams(ctx context.Context) ([]rcollab.ManagedTeam, error) {
	return c.managedTeams(ctx, managedTeamsSQL, managedLinksSQL)
}

// buzzTeamsSQL lists the managed teams whose orchestration agent runs Buzz; there is no board key to wait for.
const buzzTeamsSQL = `SELECT c.team_id, c.agent_id, coalesce(ars.phase = 'Ready', false)
FROM afs.team_collaboration c
JOIN afs.teams t ON t.id = c.team_id AND t.deleted_at IS NULL AND t.state NOT IN ('deleting', 'deleted')
JOIN afs.agents a ON a.team_id = c.team_id AND a.id = c.agent_id AND a.deleted_at IS NULL
LEFT JOIN afs.resource_status ars ON ars.resource_kind = 'agent' AND ars.resource_id = c.agent_id
WHERE c.registration_mode = 'managed' AND c.tool = 'buzz'
ORDER BY c.team_id`

// buzzLinksSQL is managedLinksSQL with the Buzz private key as the link's key.
var buzzLinksSQL = strings.ReplaceAll(managedLinksSQL, domain.SecretPaperclipAgentAPIKey, domain.SecretBuzzAgentPrivateKey)

// BuzzTeams implements collab.BuzzSource.
func (c *CollabReconcile) BuzzTeams(ctx context.Context) ([]rcollab.ManagedTeam, error) {
	return c.managedTeams(ctx, buzzTeamsSQL, buzzLinksSQL)
}

func (c *CollabReconcile) managedTeams(ctx context.Context, teamsSQL, linksSQL string) ([]rcollab.ManagedTeam, error) {
	var out []rcollab.ManagedTeam
	err := c.read(ctx, "managed collaboration", func(ctx context.Context, t *Tx) error {
		byTeam := map[string]int{}
		rows, err := t.Query(ctx, teamsSQL)
		if err != nil {
			return err
		}
		for rows.Next() {
			var mt rcollab.ManagedTeam
			if err := rows.Scan(&mt.TeamID, &mt.CollabAgentID, &mt.PodReady); err != nil {
				rows.Close()
				return err
			}
			byTeam[mt.TeamID] = len(out)
			out = append(out, mt)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		lr, err := t.Query(ctx, linksSQL)
		if err != nil {
			return err
		}
		defer lr.Close()
		gone := map[string]int{}
		for lr.Next() {
			var (
				l           rcollab.ManagedLink
				state, mode string
			)
			if err := lr.Scan(&l.TeamID, &l.AgentID, &l.AgentName, &l.Persona, &l.Connect, &l.AgentGone, &l.CollabGone, &l.Paused,
				&state, &l.PaperclipAgentID, &l.LastError, &l.KeyARN, &l.KeyPath, &mode); err != nil {
				return err
			}
			l.LinkState = domain.LinkState(state)
			if l.CollabGone {
				// Nothing left to clean when the link is already disabled and holds no id or key.
				if l.PaperclipAgentID == "" && l.KeyARN == "" && l.LinkState == domain.LinkDisabled {
					continue
				}
				i, ok := gone[l.TeamID]
				if !ok {
					i = len(out)
					gone[l.TeamID] = i
					out = append(out, rcollab.ManagedTeam{TeamID: l.TeamID, PodReady: true, Gone: true})
				}
				out[i].Links = append(out[i].Links, l)
				continue
			}
			if i, ok := byTeam[l.TeamID]; ok && domain.RegistrationMode(mode) == domain.RegistrationManaged {
				out[i].Links = append(out[i].Links, l)
			}
		}
		return lr.Err()
	})
	return out, err
}

// SetManaged implements collab.ManagedSource. A sanitized error is stored cut to the column's 500 characters.
func (c *CollabReconcile) SetManaged(ctx context.Context, teamID, agentID string, u rcollab.ManagedUpdate) error {
	if len(u.LastError) > 500 {
		u.LastError = u.LastError[:500]
	}
	return c.write(ctx, "collaboration link", func(ctx context.Context, t *Tx) error {
		_, err := t.Exec(ctx, `UPDATE afs.agent_collaboration SET link_state = $3, paperclip_agent_id = nullif($4, ''), last_synced_at = $5, last_error = nullif($6, '')
WHERE team_id = $1 AND agent_id = $2`, teamID, agentID, string(u.State), u.PaperclipAgentID, u.At, u.LastError)
		return err
	})
}

// RecordKey implements collab.ManagedSource with afs.record_collab_agent_key.
func (c *CollabReconcile) RecordKey(ctx context.Context, teamID, agentID, arn string) error {
	return c.write(ctx, "collaboration agent key", func(ctx context.Context, t *Tx) error {
		var id string
		return t.QueryRow(ctx, `SELECT afs.record_collab_agent_key($1, $2, $3)`, teamID, agentID, arn).Scan(&id)
	})
}

// RemoveKey implements collab.ManagedSource with afs.remove_collab_agent_key.
func (c *CollabReconcile) RemoveKey(ctx context.Context, teamID, agentID string) (bool, error) {
	var removed bool
	err := c.write(ctx, "collaboration agent key", func(ctx context.Context, t *Tx) error {
		return t.QueryRow(ctx, `SELECT afs.remove_collab_agent_key($1, $2)`, teamID, agentID).Scan(&removed)
	})
	return removed, err
}

// People implements collab.PeopleSource: people with a user binding on the team (viewers as viewers, the rest as
// members) and people who signed in through the edge in the last 30 days (members; this is how people who reach
// the team only through an IdP group are invited, COL-33). Only managed teams with a recorded board key and a
// ready pod are returned.
func (c *CollabReconcile) People(ctx context.Context) ([]rcollab.TeamPeople, error) {
	var out []rcollab.TeamPeople
	err := c.read(ctx, "collaboration people", func(ctx context.Context, t *Tx) error {
		rows, err := t.Query(ctx, `WITH ready AS (
  SELECT c.team_id FROM afs.team_collaboration c
  JOIN afs.agents a ON a.team_id = c.team_id AND a.id = c.agent_id AND a.deleted_at IS NULL
  JOIN afs.teams t ON t.id = c.team_id AND t.deleted_at IS NULL AND t.state = 'active'
  JOIN afs.resource_status ars ON ars.resource_kind = 'agent' AND ars.resource_id = c.agent_id AND ars.phase = 'Ready'
  WHERE c.registration_mode = 'managed'
    AND EXISTS (SELECT 1 FROM afs.secrets s WHERE s.team_id = c.team_id AND s.agent_id = c.agent_id AND s.name = '`+domain.SecretPaperclipBoardAPIKey+`')
), people AS (
  SELECT b.team_id, lower(u.email) AS email, CASE b.role WHEN 'team_viewer' THEN 'viewer' ELSE 'member' END AS role
    FROM afs.role_bindings b JOIN afs.users u ON u.id = b.user_id
   WHERE b.team_id IS NOT NULL AND b.subject_type = 'user' AND b.role IN ('team_admin', 'team_viewer', 'team_collaborator') AND u.disabled_at IS NULL
  UNION
  SELECT x.team_id, lower(u.email), 'member'
    FROM afs.collaboration_access x JOIN afs.users u ON u.id = x.principal_id
   WHERE x.principal_kind = 'user' AND x.outcome = 'signed_in' AND x.bucket_start > now() - interval '30 days' AND u.disabled_at IS NULL
)
SELECT DISTINCT ON (p.team_id, p.email) p.team_id, p.email, p.role
FROM people p JOIN ready r ON r.team_id = p.team_id
ORDER BY p.team_id, p.email, p.role`)
		if err != nil {
			return err
		}
		defer rows.Close()
		idx := map[string]int{}
		for rows.Next() {
			var team, email, role string
			if err := rows.Scan(&team, &email, &role); err != nil {
				return err
			}
			i, ok := idx[team]
			if !ok {
				i = len(out)
				idx[team] = i
				out = append(out, rcollab.TeamPeople{TeamID: team})
			}
			out[i].Wanted = append(out[i].Wanted, rcollab.Person{Email: email, Role: rcollab.InviteRole(role)})
		}
		return rows.Err()
	})
	return out, err
}

// CreatorEmail implements collab.Admins: the email of the person who created the agent, "" when the creator was
// not a person (an API key or the system).
func (c *CollabReconcile) CreatorEmail(ctx context.Context, teamID, agentID string) (string, error) {
	var email string
	err := c.read(ctx, "collaboration agent creator", func(ctx context.Context, t *Tx) error {
		err := t.QueryRow(ctx, `SELECT u.email FROM afs.agents a JOIN afs.users u ON u.id = a.created_by WHERE a.team_id = $1 AND a.id = $2`,
			teamID, agentID).Scan(&email)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return email, err
}

// PaperclipTarget is where a team's Paperclip is and the secret that holds its board key.
type PaperclipTarget struct {
	Namespace   string
	BoardKeyARN string
}

// PaperclipTarget returns the namespace of the team and the recorded board key ARN ("" when none).
func (c *CollabReconcile) PaperclipTarget(ctx context.Context, teamID string) (PaperclipTarget, error) {
	var out PaperclipTarget
	err := c.read(ctx, "paperclip target", func(ctx context.Context, t *Tx) error {
		err := t.QueryRow(ctx, `SELECT t.namespace, coalesce(s.secrets_manager_arn, '')
FROM afs.team_collaboration c
JOIN afs.teams t ON t.id = c.team_id
LEFT JOIN afs.secrets s ON s.team_id = c.team_id AND s.agent_id = c.agent_id AND s.name = '`+domain.SecretPaperclipBoardAPIKey+`'
WHERE c.team_id = $1`, teamID).Scan(&out.Namespace, &out.BoardKeyARN)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("collaboration")
		}
		return err
	})
	return out, err
}

// SecretPath returns the Secrets Manager name afs.secret_path gives the agent's secret called name.
func (c *CollabReconcile) SecretPath(ctx context.Context, teamID, agentID, name string) (string, error) {
	var p string
	err := c.read(ctx, "secret path", func(ctx context.Context, t *Tx) error {
		return t.QueryRow(ctx, `SELECT afs.secret_path($1, $2, $3)`, teamID, agentID, name).Scan(&p)
	})
	return p, err
}
