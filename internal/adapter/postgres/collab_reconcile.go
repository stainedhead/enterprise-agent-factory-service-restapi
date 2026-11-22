package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	rcollab "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

// CollabReconcile implements the database ports of the collaboration reconciler jobs (RC-21..29):
// the Source, the LinkSource, the AccessSource, the Events, the BoardKeys and the Housekeeping.
// Every call runs as the reconciler session (role afs_reconciler).
type CollabReconcile struct {
	runner *TxRunner
	outbox port.OutboxWriter
	ids    port.IDGen
	clock  port.Clock
	finder BoardKeyFinder
}

// BoardKeyFinder finds the board key secret in Secrets Manager by name. It never reads a value.
type BoardKeyFinder interface {
	Find(ctx context.Context, teamID, agentID string) (arn string, found bool, err error)
}

// NewCollabReconcile returns the adapter. finder may be nil: no board key is then ever found, which
// leaves managed bootstrap pending (recorded as a parked item: the Secrets Manager finder).
func NewCollabReconcile(runner *TxRunner, outbox port.OutboxWriter, ids port.IDGen, clock port.Clock, finder BoardKeyFinder) *CollabReconcile {
	return &CollabReconcile{runner: runner, outbox: outbox, ids: ids, clock: clock, finder: finder}
}

var (
	_ rcollab.Source       = (*CollabReconcile)(nil)
	_ rcollab.LinkSource   = (*CollabReconcile)(nil)
	_ rcollab.AccessSource = (*CollabReconcile)(nil)
	_ rcollab.Events       = (*CollabReconcile)(nil)
	_ rcollab.BoardKeys    = (*CollabReconcile)(nil)
	_ rcollab.Housekeeping = (*CollabReconcile)(nil)
	_ rcollab.CIDRSource   = (*CollabReconcile)(nil)
)

func (c *CollabReconcile) read(ctx context.Context, what string, fn func(context.Context, *Tx) error) error {
	err := c.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return fn(ctx, t)
	})
	return MapError(err, OpRead, what)
}

func (c *CollabReconcile) write(ctx context.Context, what string, fn func(context.Context, *Tx) error) error {
	err := c.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return fn(ctx, t)
	})
	return MapError(err, OpWrite, what)
}

const collabStatesSQL = `SELECT c.team_id, c.agent_id, t.slug, t.state, a.state, c.web_ui_enabled, c.rest_api_enabled, c.registration_mode,
  t.may_expose_collaboration, coalesce(p.collab_client_id, '') <> '', c.generation,
  coalesce(ars.phase = 'Ready', false), rs.conditions
FROM afs.team_collaboration c
JOIN afs.teams t ON t.id = c.team_id AND t.deleted_at IS NULL AND t.state NOT IN ('deleting', 'deleted')
JOIN afs.agents a ON a.team_id = c.team_id AND a.id = c.agent_id AND a.deleted_at IS NULL
LEFT JOIN afs.identity_providers p ON p.id = coalesce(t.identity_provider_id, (SELECT d.id FROM afs.identity_providers d WHERE d.is_default))
LEFT JOIN afs.resource_status ars ON ars.resource_kind = 'agent' AND ars.resource_id = c.agent_id
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + CollaborationStatusKind + `' AND rs.resource_id = c.team_id
ORDER BY c.team_id`

// States implements collab.Source.
func (c *CollabReconcile) States(ctx context.Context) ([]rcollab.TeamState, error) {
	set, err := c.settings(ctx)
	if err != nil {
		return nil, err
	}
	var out []rcollab.TeamState
	err = c.read(ctx, "collaboration states", func(ctx context.Context, t *Tx) error {
		rows, err := t.Query(ctx, collabStatesSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				s     rcollab.TeamState
				mode  string
				conds []byte
				gen   int64
			)
			if err := rows.Scan(&s.TeamID, &s.AgentID, &s.TeamSlug, &s.TeamState, &s.AgentState, &s.WebUIEnabled, &s.RESTAPIEnabled, &mode,
				&s.MayExpose, &s.ProviderClient, &gen, &s.PodReady, &conds); err != nil {
				return err
			}
			s.Mode, s.Generation = domain.RegistrationMode(mode), gen
			s.BaseDomain, s.PublicEndpoints = set.BaseDomain, set.PublicEndpoints
			if len(conds) > 0 {
				var cs []domain.Condition
				if err := json.Unmarshal(conds, &cs); err != nil {
					return err
				}
				applyPrevious(&s, cs)
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// applyPrevious copies the last written conditions into the state so the controller emits only on change.
func applyPrevious(s *rcollab.TeamState, cs []domain.Condition) {
	for _, cd := range cs {
		on := cd.Status == domain.ConditionTrue
		switch cd.Type {
		case rcollab.ConditionEndpointReady:
			s.PrevReady, s.PrevReason = &on, cd.Reason
		case rcollab.ConditionPaperclipReady:
			s.PrevPaperclip = &on
		}
	}
	s.Unmanaged = unmanagedNames(cs)
}

func (c *CollabReconcile) settings(ctx context.Context) (domain.CollabSettings, error) {
	var out domain.CollabSettings
	rd := NewSettingsReader(c.runner)
	raw, err := rd.Setting(ctx, domain.SettingCollabBaseDomain)
	if err != nil {
		return out, err
	}
	var base *string
	if err := json.Unmarshal(raw, &base); err != nil {
		return out, domain.Internal(err)
	}
	if base != nil {
		out.BaseDomain = *base
	}
	raw, err = rd.Setting(ctx, domain.SettingCollabPublicEndpoints)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out.PublicEndpoints); err != nil {
		return out, domain.Internal(err)
	}
	return out, nil
}

const collabLinksSQL = `SELECT ac.team_id, ac.agent_id, ac.connect, ac.link_state, coalesce(ac.paperclip_agent_id, ''),
  EXISTS (SELECT 1 FROM afs.secrets s WHERE s.team_id = ac.team_id AND s.agent_id = ac.agent_id AND s.name = '` + domain.SecretPaperclipAgentAPIKey + `'
          AND s.current_version_id IS NOT NULL),
  tc.team_id IS NULL OR ca.id IS NULL, a.id IS NULL OR a.deleted_at IS NOT NULL, coalesce(tc.registration_mode, 'manual')
FROM afs.agent_collaboration ac
LEFT JOIN afs.agents a ON a.team_id = ac.team_id AND a.id = ac.agent_id
LEFT JOIN afs.team_collaboration tc ON tc.team_id = ac.team_id
LEFT JOIN afs.agents ca ON ca.team_id = tc.team_id AND ca.id = tc.agent_id AND ca.deleted_at IS NULL
ORDER BY ac.team_id, ac.agent_id`

// Links implements collab.LinkSource.
func (c *CollabReconcile) Links(ctx context.Context) ([]rcollab.Link, error) {
	var out []rcollab.Link
	err := c.read(ctx, "collaboration links", func(ctx context.Context, t *Tx) error {
		rows, err := t.Query(ctx, collabLinksSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				l           rcollab.Link
				state, mode string
			)
			if err := rows.Scan(&l.TeamID, &l.AgentID, &l.Connect, &state, &l.PaperclipAgentID, &l.KeyPresent, &l.CollabAgentGone, &l.AgentGone, &mode); err != nil {
				return err
			}
			l.LinkState, l.Mode = domain.LinkState(state), domain.RegistrationMode(mode)
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// SetState implements collab.LinkSource. A sanitized error is stored truncated to the column's 500 characters.
func (c *CollabReconcile) SetState(ctx context.Context, teamID, agentID string, st domain.LinkState, lastErr string, at time.Time) error {
	if len(lastErr) > 500 {
		lastErr = lastErr[:500]
	}
	return c.write(ctx, "collaboration link", func(ctx context.Context, t *Tx) error {
		_, err := t.Exec(ctx, `UPDATE afs.agent_collaboration SET link_state = $3, last_synced_at = $4, last_error = nullif($5, '')
WHERE team_id = $1 AND agent_id = $2`, teamID, agentID, string(st), at, lastErr)
		return err
	})
}

// DeniedSince implements collab.AccessSource.
func (c *CollabReconcile) DeniedSince(ctx context.Context, since time.Time) ([]rcollab.Denied, error) {
	var out []rcollab.Denied
	err := c.read(ctx, "collaboration access", func(ctx context.Context, t *Tx) error {
		rows, err := t.Query(ctx, `SELECT team_id,
  coalesce(sum(requests) FILTER (WHERE outcome = 'denied_unauthenticated'), 0),
  coalesce(sum(requests) FILTER (WHERE outcome = 'denied_forbidden'), 0),
  coalesce(sum(requests) FILTER (WHERE outcome = 'denied_surface_disabled'), 0),
  coalesce(sum(requests) FILTER (WHERE outcome = 'denied_secret_detected'), 0)
FROM afs.collaboration_access WHERE bucket_start >= $1 GROUP BY team_id`, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d rcollab.Denied
			if err := rows.Scan(&d.TeamID, &d.Unauthenticated, &d.Forbidden, &d.SurfaceDisabled, &d.SecretDetected); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// AllowedCIDRs implements collab.CIDRSource: collab.allowed_source_cidrs.
func (c *CollabReconcile) AllowedCIDRs(ctx context.Context) ([]string, error) {
	raw, err := NewSettingsReader(c.runner).Setting(ctx, domain.SettingCollabAllowedSourceCidrs)
	if err != nil {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, domain.Internal(err)
	}
	return out, nil
}

// Threshold implements collab.AccessSource.
func (c *CollabReconcile) Threshold(ctx context.Context) (int, error) {
	raw, err := NewSettingsReader(c.runner).Setting(ctx, domain.SettingCollabDeniedSpikeThreshold)
	if err != nil {
		return 0, err
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, domain.Internal(err)
	}
	return n, nil
}

// LastSpike implements collab.AccessSource: the last collaboration.denied_spike of the team in the outbox.
func (c *CollabReconcile) LastSpike(ctx context.Context, teamID string) (time.Time, error) {
	var at *time.Time
	err := c.read(ctx, "outbox", func(ctx context.Context, t *Tx) error {
		return t.QueryRow(ctx, `SELECT max(created_at) FROM afs.outbox_events WHERE event_type = $1 AND team_id = $2`,
			string(domain.EventCollaborationDeniedSpike), teamID).Scan(&at)
	})
	if at == nil {
		return time.Time{}, err
	}
	return *at, err
}

// Publish implements collab.Events: one event on the outbox in its own transaction.
func (c *CollabReconcile) Publish(ctx context.Context, typ domain.EventType, teamID, kind, id, agentID string, data map[string]any) error {
	return c.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		eid, err := c.ids.NewID(eventIDPrefix)
		if err != nil {
			return err
		}
		ev, err := domain.NewEvent(typ, eid.String(), c.clock.Now(), tx.Session().Actor, teamID, kind, id, data)
		if err != nil {
			return err
		}
		ev.Detail.AgentID = agentID
		return c.outbox.Publish(ctx, tx, ev)
	})
}

// Pending implements collab.BoardKeys: every collaboration agent in managed mode, with whether its board key
// is recorded and whether its pod is ready.
func (c *CollabReconcile) Pending(ctx context.Context) ([]rcollab.BoardKeyCandidate, error) {
	var out []rcollab.BoardKeyCandidate
	err := c.read(ctx, "board keys", func(ctx context.Context, t *Tx) error {
		rows, err := t.Query(ctx, `SELECT c.team_id, c.agent_id, c.registration_mode,
  EXISTS (SELECT 1 FROM afs.secrets s WHERE s.team_id = c.team_id AND s.agent_id = c.agent_id AND s.name = '`+domain.SecretPaperclipBoardAPIKey+`'),
  coalesce(ars.phase = 'Ready', false)
FROM afs.team_collaboration c
JOIN afs.teams t ON t.id = c.team_id AND t.deleted_at IS NULL AND t.state NOT IN ('deleting', 'deleted')
JOIN afs.agents a ON a.team_id = c.team_id AND a.id = c.agent_id AND a.deleted_at IS NULL
LEFT JOIN afs.resource_status ars ON ars.resource_kind = 'agent' AND ars.resource_id = c.agent_id
WHERE c.registration_mode = 'managed' ORDER BY c.team_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b rcollab.BoardKeyCandidate
			var mode string
			if err := rows.Scan(&b.TeamID, &b.AgentID, &mode, &b.Recorded, &b.PodReady); err != nil {
				return err
			}
			b.Mode = domain.RegistrationMode(mode)
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// Find implements collab.BoardKeys through the configured finder.
func (c *CollabReconcile) Find(ctx context.Context, teamID, agentID string) (string, bool, error) {
	if c.finder == nil {
		return "", false, nil
	}
	return c.finder.Find(ctx, teamID, agentID)
}

// Record implements collab.BoardKeys with afs.record_collab_board_key.
func (c *CollabReconcile) Record(ctx context.Context, teamID, agentID, arn string) error {
	return c.write(ctx, "board key", func(ctx context.Context, t *Tx) error {
		var id string
		return t.QueryRow(ctx, `SELECT afs.record_collab_board_key($1, $2, $3)`, teamID, agentID, arn).Scan(&id)
	})
}

// EnsureAccessPartition implements collab.Housekeeping.
func (c *CollabReconcile) EnsureAccessPartition(ctx context.Context, month string) error {
	return c.write(ctx, "access partition", func(ctx context.Context, t *Tx) error {
		var name string
		return t.QueryRow(ctx, `SELECT afs.ensure_collaboration_access_partition($1::date)`, month).Scan(&name)
	})
}

// PurgeEdgeSessions implements collab.Housekeeping.
func (c *CollabReconcile) PurgeEdgeSessions(ctx context.Context) (int64, error) {
	var n int64
	err := c.write(ctx, "edge sessions", func(ctx context.Context, t *Tx) error {
		err := t.QueryRow(ctx, `SELECT afs.purge_collab_edge_sessions()`).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return n, err
}
