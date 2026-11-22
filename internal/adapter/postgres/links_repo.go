package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
)

// LinksStore implements links.Source and links.Recorder for the reconciler login (afs_reconciler may
// update status, last_verified_at and last_error of a role link and append audit and outbox rows).
type LinksStore struct {
	runner *TxRunner
	audit  port.AuditWriter
	outbox port.OutboxWriter
	ids    port.IDGen
}

// NewLinksStore returns a LinksStore on the reconciler's pool.
func NewLinksStore(runner *TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen) *LinksStore {
	return &LinksStore{runner: runner, audit: audit, outbox: outbox, ids: ids}
}

var (
	_ links.Source   = (*LinksStore)(nil)
	_ links.Recorder = (*LinksStore)(nil)
)

// AuditLinkStatus is the audit action of a status the reconciler wrote to a role link.
const AuditLinkStatus = "ReconcileRoleLink"

// LoadLinks implements links.Source: the team's state and every link with its agent's state and
// whether the team's filter allows its account (afs.account_allowed).
func (s *LinksStore) LoadLinks(ctx context.Context, teamID string) (links.Desired, error) {
	d := links.Desired{TeamID: teamID}
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var ns string
		if err := t.QueryRow(ctx, `SELECT state, namespace FROM afs.teams WHERE id = $1`, teamID).Scan(&d.State, &ns); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("team")
			}
			return err
		}
		rows, err := t.Query(ctx, `SELECT l.id, l.agent_id, l.account_id, l.role_arn, l.profile_name, l.external_id, l.max_session_seconds,
       l.status, l.last_verified_at, l.updated_at, afs.account_allowed(l.team_id, l.account_id),
       a.iam_identity = 'dedicated', a.state IN ('enabled', 'paused') AND a.deleted_at IS NULL
  FROM afs.agent_role_links l JOIN afs.agents a ON a.id = l.agent_id
 WHERE l.team_id = $1 ORDER BY l.id`, teamID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l links.Link
			var status string
			if err := rows.Scan(&l.ID, &l.AgentID, &l.AccountID, &l.RoleARN, &l.ProfileName, &l.ExternalID, &l.MaxSession, &status,
				&l.LastVerified, &l.UpdatedAt, &l.Allowed, &l.AgentDedicated, &l.AgentLive); err != nil {
				return err
			}
			l.Status, l.Namespace = domain.RoleLinkStatus(status), ns
			d.Links = append(d.Links, l)
		}
		return rows.Err()
	})
	return d, MapError(err, OpRead, "role links")
}

// Record implements links.Recorder: the status columns, one audit record and, for a transition into
// verified or failed, the contract event, in one transaction. The link row is only changed while it
// still holds the status the pass saw (From), so a verify requested meanwhile is not overwritten.
func (s *LinksStore) Record(ctx context.Context, c links.Change) error {
	err := s.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var lastErr *string
		if c.Error != "" {
			lastErr = &c.Error
		}
		tag, err := t.Exec(ctx, `UPDATE afs.agent_role_links SET status = $2, last_error = $3,
       last_verified_at = CASE WHEN $4::timestamptz IS NULL THEN last_verified_at ELSE $4 END
 WHERE id = $1 AND status = $5`, c.LinkID, string(c.To), lastErr, c.VerifiedAt, string(c.From))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // the link changed or went away since the pass loaded it; the next pass sees it
		}
		if err := s.audit.Write(ctx, tx, port.AuditRecord{Action: AuditLinkStatus, Outcome: port.OutcomeAllowed,
			ResourceKind: "agent_role_links", ResourceID: c.LinkID, TeamID: c.TeamID,
			Before: map[string]any{"status": string(c.From)},
			After:  map[string]any{"status": string(c.To), "error": c.Error, "agentId": c.AgentID, "accountId": c.AccountID}}); err != nil {
			return err
		}
		if c.Event == "" {
			return nil
		}
		id, err := s.ids.NewID(eventIDPrefix)
		if err != nil {
			return err
		}
		data := map[string]any{"linkId": c.LinkID, "agentId": c.AgentID, "accountId": c.AccountID, "roleArn": c.RoleARN, "status": string(c.To)}
		if c.Error != "" {
			data["error"] = c.Error
		}
		ev, err := domain.NewEvent(c.Event, id.String(), time.Now().UTC(), tx.Session().Actor, c.TeamID, "agent_role_link", c.LinkID, data)
		if err != nil {
			return err
		}
		return s.outbox.Publish(ctx, tx, ev)
	})
	return MapError(err, OpWrite, "role link")
}
