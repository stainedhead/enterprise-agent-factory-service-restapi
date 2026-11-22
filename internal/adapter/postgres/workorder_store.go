package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

// WorkOrderStore is the reconciler's database access for work orders (M6-E): intake, team creation, template
// secrets, tracking and owner notifications. It implements workorder.IntakeStore, AssignStore, SecretStore,
// TrackStore and NotifyStore for the afs_reconciler login, whose privileges are INSERT and UPDATE of
// (status, team_id, assigned_at, completed_at) on work_orders, INSERT and UPDATE of (status, pr_url, pr_number,
// detail, merged_at) on work_order_repos, INSERT on work_order_notifications, audit and outbox, and EXECUTE on
// afs.instantiate_work_order_team and afs.resolve_repo_owners.
type WorkOrderStore struct {
	runner port.TxRunner
	audit  port.AuditWriter
	outbox port.OutboxWriter
	ids    port.IDGen
	now    func() time.Time
}

// NewWorkOrderStore returns a WorkOrderStore; now stamps events.
func NewWorkOrderStore(runner port.TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen, now func() time.Time) *WorkOrderStore {
	return &WorkOrderStore{runner: runner, audit: audit, outbox: outbox, ids: ids, now: now}
}

var (
	_ workorder.IntakeStore = (*WorkOrderStore)(nil)
	_ workorder.AssignStore = (*WorkOrderStore)(nil)
	_ workorder.SecretStore = (*WorkOrderStore)(nil)
	_ workorder.TrackStore  = (*WorkOrderStore)(nil)
	_ workorder.NotifyStore = (*WorkOrderStore)(nil)
)

const (
	cveOpenIndex    = "work_orders_open_cve_uq"
	eventOnceIndex  = "work_orders_event_once"
	kindWorkOrders  = "work_orders"
	teamStateOKList = "('active', 'suspended')"
)

// ---- intake ----------------------------------------------------------------------------------------------

// CreateFromEvent implements workorder.IntakeStore.
func (s *WorkOrderStore) CreateFromEvent(ctx context.Context, n workorder.NewWorkOrder, at time.Time) (workorder.CreateResult, error) {
	var res workorder.CreateResult
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		res = workorder.CreateResult{}
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		if err := t.QueryRow(ctx, `SELECT id FROM afs.work_orders WHERE source = 'eventbridge' AND source_event_id = $1`, n.SourceEventID).Scan(&res.ID); err == nil {
			res.Duplicate = true
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := s.checkTemplate(ctx, t, n.TemplateID); err != nil {
			return err
		}
		if n.Kind == "cve_remediation" {
			var open string
			err := t.QueryRow(ctx, `SELECT id FROM afs.work_orders WHERE kind = 'cve_remediation' AND external_ref = $1
 AND status NOT IN ('done', 'failed', 'cancelled')`, n.CVEID).Scan(&open)
			if err == nil {
				return &workorder.Rejection{Reason: "open_work_order", Detail: map[string]any{"cveId": n.CVEID, "workOrderId": open}}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		repos, err := s.resolveOwners(ctx, t, n.Repos)
		if err != nil {
			return err
		}
		id, err := s.ids.NewID(domain.PrefixWorkOrder)
		if err != nil {
			return err
		}
		res.ID = id.String()
		var budget any
		if n.Budget.Micros() > 0 {
			budget = n.Budget.String()
		}
		if _, err := t.Exec(ctx, `INSERT INTO afs.work_orders
  (id, kind, external_ref, title, payload, sla_due_at, status, severity, source, source_event_id, template_id, budget_usd)
 VALUES ($1, $2, nullif($3, ''), $4, jsonb_build_object('sourceEventId', $5::text), $6, 'open', $7, 'eventbridge', $5, $8, $9::numeric)`,
			res.ID, n.Kind, n.CVEID, n.Title, n.SourceEventID, n.SLADueAt, n.Severity, n.TemplateID, budget); err != nil {
			return err
		}
		for _, r := range repos {
			if _, err := t.Exec(ctx, `INSERT INTO afs.work_order_repos (work_order_id, host, repo, owning_team_id, owning_contact)
 VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''))`, res.ID, r.Host, r.Repo, r.OwningTeamID, r.OwningContact); err != nil {
				return err
			}
		}
		if err := s.audit.Write(ctx, tx, port.AuditRecord{Action: "CreateWorkOrder", Outcome: port.OutcomeAllowed, ResourceKind: kindWorkOrders, ResourceID: res.ID,
			After: map[string]any{"source": "eventbridge", "sourceEventId": n.SourceEventID, "kind": n.Kind, "cveId": n.CVEID, "templateId": n.TemplateID, "repos": len(repos)}}); err != nil {
			return err
		}
		return s.publish(ctx, tx, domain.EventWorkOrderCreated, "", res.ID, at, map[string]any{
			"workOrderId": res.ID, "kind": n.Kind, "cveId": n.CVEID, "severity": n.Severity, "source": "eventbridge", "repos": len(repos)})
	})
	if err != nil {
		var rej *workorder.Rejection
		if errors.As(err, &rej) {
			return workorder.CreateResult{}, rej
		}
		// A concurrent intake of the same event or CVE loses on the unique index; the rolled-back transaction
		// wrote nothing, so the answer is the one the winner's row gives.
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == codeUnique {
			switch pe.ConstraintName {
			case eventOnceIndex:
				return s.existing(ctx, n.SourceEventID)
			case cveOpenIndex:
				return workorder.CreateResult{}, &workorder.Rejection{Reason: "open_work_order", Detail: map[string]any{"cveId": n.CVEID}}
			}
		}
		return workorder.CreateResult{}, MapError(err, OpWrite, "work order")
	}
	return res, nil
}

func (s *WorkOrderStore) existing(ctx context.Context, eventID string) (workorder.CreateResult, error) {
	var res workorder.CreateResult
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT id FROM afs.work_orders WHERE source = 'eventbridge' AND source_event_id = $1`, eventID).Scan(&res.ID)
	})
	res.Duplicate = true
	return res, MapError(err, OpRead, "work order")
}

func (s *WorkOrderStore) checkTemplate(ctx context.Context, t *Tx, id string) error {
	var enabled bool
	err := t.QueryRow(ctx, `SELECT enabled FROM afs.swarm_templates WHERE id = $1`, id).Scan(&enabled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return &workorder.Rejection{Reason: "template_unavailable", Detail: map[string]any{"templateId": id, "why": "unknown"}}
	case err != nil:
		return err
	case !enabled:
		return &workorder.Rejection{Reason: "template_unavailable", Detail: map[string]any{"templateId": id, "why": "disabled"}}
	}
	return nil
}

// resolveOwners gives every repository an owner: an explicit one stays (a named team must exist and not be
// deleted), and a repository without one is resolved through afs.resolve_repo_owners (D38): exactly one team at
// the best match, else the whole event is refused, listing every repository that failed.
func (s *WorkOrderStore) resolveOwners(ctx context.Context, t *Tx, in []workorder.RepoInput) ([]workorder.RepoInput, error) {
	out := make([]workorder.RepoInput, 0, len(in))
	var unowned, unknown []string
	var ambiguous []map[string]any
	for _, r := range in {
		switch {
		case r.OwningTeamID != "":
			var ok bool
			if err := t.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.teams WHERE id = $1 AND state IN `+teamStateOKList+`)`, r.OwningTeamID).Scan(&ok); err != nil {
				return nil, err
			}
			if !ok {
				unknown = append(unknown, r.Host+"/"+r.Repo)
				continue
			}
		case r.OwningContact == "":
			var teams []string
			if err := collect(ctx, t, "repository owners", `SELECT afs.resolve_repo_owners($1, $2)`, []any{r.Host, r.Repo}, func(rows pgx.Rows) error {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				teams = append(teams, id)
				return nil
			}); err != nil {
				return nil, err
			}
			switch len(teams) {
			case 0:
				unowned = append(unowned, r.Host+"/"+r.Repo)
				continue
			case 1:
				r.OwningTeamID = teams[0]
			default:
				sort.Strings(teams)
				ambiguous = append(ambiguous, map[string]any{"repo": r.Host + "/" + r.Repo, "teamIds": teams})
				continue
			}
		}
		out = append(out, r)
	}
	switch {
	case len(unknown) > 0:
		return nil, &workorder.Rejection{Reason: "unknown_owner", Detail: map[string]any{"repos": unknown}}
	case len(ambiguous) > 0:
		d := map[string]any{"ambiguousRepos": ambiguous}
		if len(unowned) > 0 {
			d["unownedRepos"] = unowned
		}
		return nil, &workorder.Rejection{Reason: "ambiguous_repos", Detail: d}
	case len(unowned) > 0:
		return nil, &workorder.Rejection{Reason: "unowned_repos", Detail: map[string]any{"unownedRepos": unowned}}
	}
	return out, nil
}

// Reject implements workorder.IntakeStore: the refusal is one audit record, with the reason and the details
// (repositories, ids; never the event body).
func (s *WorkOrderStore) Reject(ctx context.Context, id string, r *workorder.Rejection) error {
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		return s.audit.Write(ctx, tx, port.AuditRecord{Action: "work_order.intake_refused", Outcome: port.OutcomeFailed, ResourceKind: kindWorkOrders, ResourceID: id,
			After: map[string]any{"reason": r.Reason, "detail": r.Detail}})
	})
	return MapError(err, OpWrite, "audit record")
}

// ---- assignment ------------------------------------------------------------------------------------------

// Unassigned implements workorder.AssignStore.
func (s *WorkOrderStore) Unassigned(ctx context.Context) ([]string, error) {
	var out []string
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return collect(ctx, t, "open work orders", `SELECT id FROM afs.work_orders WHERE status = 'open' AND team_id IS NULL AND template_id IS NOT NULL
 ORDER BY created_at, id LIMIT 50`, nil, func(rows pgx.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
			return nil
		})
	})
	return out, err
}

// Instantiate implements workorder.AssignStore through afs.instantiate_work_order_team (X-DB-4).
func (s *WorkOrderStore) Instantiate(ctx context.Context, id string) (string, error) {
	var team string
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT afs.instantiate_work_order_team($1)`, id).Scan(&team)
	})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return "", fmt.Errorf("instantiate work order team: %s (%s)", pe.Message, pe.Code)
		}
		return "", MapError(err, OpWrite, "remediation team")
	}
	return team, nil
}

// ---- template secrets ------------------------------------------------------------------------------------

const recordSecretSig = `afs.record_work_order_secret(text,text,text,text,text,text,text)`

// Supported implements workorder.SecretStore.
func (s *WorkOrderStore) Supported(ctx context.Context) (bool, error) {
	var ok bool
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT to_regprocedure('`+recordSecretSig+`') IS NOT NULL`).Scan(&ok)
	})
	return ok, MapError(err, OpRead, "work order secrets")
}

// Pending implements workorder.SecretStore.
func (s *WorkOrderStore) Pending(ctx context.Context) ([]workorder.TemplateSecret, error) {
	var out []workorder.TemplateSecret
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return collect(ctx, t, "template secrets", `
SELECT w.id, w.team_id, p.id, p.name, e ->> 'name', coalesce(e ->> 'description', ''), coalesce(e ->> 'delivery', ''), coalesce(e ->> 'target', ''),
       afs.secret_path(w.team_id, NULL, p.id, e ->> 'name')
  FROM afs.work_orders w
  JOIN afs.swarm_templates st ON st.id = w.template_id
  CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(st.spec -> 'secrets') = 'array' THEN st.spec -> 'secrets' ELSE '[]'::jsonb END) e
  JOIN afs.agent_pools p ON p.team_id = w.team_id AND p.name = e ->> 'pool' AND p.state IN ('enabled', 'paused')
 WHERE w.status IN ('assigned', 'in_progress', 'pr_opened') AND w.team_id IS NOT NULL
   AND jsonb_typeof(e) = 'object' AND e ->> 'name' ~ '^[a-z][a-z0-9-]{0,62}$'
   AND NOT EXISTS (SELECT 1 FROM afs.secrets x WHERE x.team_id = w.team_id AND x.name = e ->> 'name')
 ORDER BY w.id, p.name, e ->> 'name'`, nil, func(rows pgx.Rows) error {
			var ts workorder.TemplateSecret
			if err := rows.Scan(&ts.WorkOrderID, &ts.TeamID, &ts.PoolID, &ts.Pool, &ts.Name, &ts.Description, &ts.Delivery, &ts.Target, &ts.Path); err != nil {
				return err
			}
			out = append(out, ts)
			return nil
		})
	})
	return out, err
}

// Record implements workorder.SecretStore through afs.record_work_order_secret (ProposedRecordWorkOrderSecretSQL).
func (s *WorkOrderStore) Record(ctx context.Context, ts workorder.TemplateSecret, arn string) error {
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var id string
		return t.QueryRow(ctx, `SELECT afs.record_work_order_secret($1, $2, $3, $4, $5, $6, $7)`,
			ts.WorkOrderID, ts.PoolID, ts.Name, ts.Description, arn, ts.Delivery, ts.Target).Scan(&id)
	})
	return MapError(err, OpWrite, "work order secret")
}

// ---- tracking --------------------------------------------------------------------------------------------

// Active implements workorder.TrackStore.
func (s *WorkOrderStore) Active(ctx context.Context) ([]workorder.WorkOrder, error) {
	var out []workorder.WorkOrder
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		idx := map[string]int{}
		if err := collect(ctx, t, "work orders", `
SELECT w.id, w.kind, coalesce(w.external_ref, ''), w.title, w.severity, w.status, coalesce(w.team_id, ''),
       coalesce(tm.state IN ('deleting', 'deleted') OR tm.torn_down_at IS NOT NULL, false),
       coalesce(w.sla_due_at, 'epoch'::timestamptz), coalesce(p.sla_state, '')
  FROM afs.work_orders w
  LEFT JOIN afs.teams tm ON tm.id = w.team_id
  LEFT JOIN afs.work_order_progress p ON p.work_order_id = w.id
 WHERE w.status IN ('open', 'assigned', 'in_progress', 'pr_opened')
    OR (w.status = 'done' AND w.completed_at > now() - interval '3 days')
 ORDER BY w.created_at, w.id`, nil, func(rows pgx.Rows) error {
			var w workorder.WorkOrder
			if err := rows.Scan(&w.ID, &w.Kind, &w.ExternalRef, &w.Title, &w.Severity, &w.Status, &w.TeamID, &w.TeamTornDown, &w.SLADueAt, &w.SLAState); err != nil {
				return err
			}
			if w.SLADueAt.Equal(time.Unix(0, 0)) {
				w.SLADueAt = time.Time{}
			}
			idx[w.ID] = len(out)
			out = append(out, w)
			return nil
		}); err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		ids := make([]string, 0, len(out))
		for _, w := range out {
			ids = append(ids, w.ID)
		}
		return collect(ctx, t, "work order repositories", `
SELECT work_order_id, host, repo, status, coalesce(pr_url, ''), coalesce(pr_number, 0), coalesce(owning_team_id, ''), coalesce(owning_contact, '')
  FROM afs.work_order_repos WHERE work_order_id = ANY($1) ORDER BY work_order_id, host, repo`, []any{ids}, func(rows pgx.Rows) error {
			var id string
			var r workorder.Repo
			if err := rows.Scan(&id, &r.Host, &r.Repo, &r.Status, &r.PRURL, &r.PRNumber, &r.OwningTeamID, &r.OwningContact); err != nil {
				return err
			}
			out[idx[id]].Repos = append(out[idx[id]].Repos, r)
			return nil
		})
	})
	return out, err
}

// ApplyRepo implements workorder.TrackStore.
func (s *WorkOrderStore) ApplyRepo(ctx context.Context, c workorder.RepoChange) (bool, error) {
	applied := false
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		applied = false
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var mergedAt any
		if c.To == workorder.RepoMerged {
			mergedAt = c.MergedAt
		}
		tag, err := t.Exec(ctx, `UPDATE afs.work_order_repos SET status = $5, detail = nullif($6, ''), merged_at = $7
 WHERE work_order_id = $1 AND host = $2 AND repo = $3 AND status = $4 AND pr_url IS NOT DISTINCT FROM nullif($8, '')`,
			c.WorkOrderID, c.Host, c.Repo, c.FromStatus, c.To, c.Detail, mergedAt, c.FromPRURL)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		applied = true
		if err := s.audit.Write(ctx, tx, port.AuditRecord{Action: "work_order.repo_updated", Outcome: port.OutcomeAllowed, ResourceKind: kindWorkOrders, ResourceID: c.WorkOrderID, TeamID: c.TeamID,
			Before: map[string]any{"repo": c.Host + "/" + c.Repo, "status": c.FromStatus}, After: map[string]any{"repo": c.Host + "/" + c.Repo, "status": c.To, "prUrl": c.PRURL, "source": "github"}}); err != nil {
			return err
		}
		data := map[string]any{"workOrderId": c.WorkOrderID, "host": c.Host, "repo": c.Repo, "status": c.To, "prUrl": c.PRURL, "source": "github"}
		if err := s.publish(ctx, tx, domain.EventWorkOrderRepoUpdated, c.TeamID, c.WorkOrderID, s.now(), data); err != nil {
			return err
		}
		if c.To == workorder.RepoMerged {
			return s.publish(ctx, tx, domain.EventWorkOrderMerged, c.TeamID, c.WorkOrderID, s.now(), data)
		}
		return nil
	})
	return applied, MapError(err, OpWrite, "work order repository")
}

// ApplyStatus implements workorder.TrackStore.
func (s *WorkOrderStore) ApplyStatus(ctx context.Context, c workorder.StatusChange) (bool, error) {
	applied := false
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		applied = false
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		tag, err := t.Exec(ctx, `UPDATE afs.work_orders SET status = $3,
 completed_at = CASE WHEN $3 IN ('done', 'failed') THEN now() ELSE completed_at END
 WHERE id = $1 AND status = $2`, c.WorkOrderID, c.From, c.To)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		applied = true
		after := map[string]any{"status": c.To}
		if c.Reason != "" {
			after["reason"] = c.Reason
		}
		if err := s.audit.Write(ctx, tx, port.AuditRecord{Action: "work_order.status_changed", Outcome: port.OutcomeAllowed, ResourceKind: kindWorkOrders, ResourceID: c.WorkOrderID, TeamID: c.TeamID,
			Before: map[string]any{"status": c.From}, After: after}); err != nil {
			return err
		}
		data := map[string]any{"workOrderId": c.WorkOrderID, "teamId": c.TeamID}
		switch c.To {
		case workorder.StatusDone:
			return s.publish(ctx, tx, domain.EventWorkOrderCompleted, c.TeamID, c.WorkOrderID, c.At, data)
		case workorder.StatusFailed:
			data["reason"] = c.Reason
			return s.publish(ctx, tx, domain.EventWorkOrderFailed, c.TeamID, c.WorkOrderID, c.At, data)
		}
		return nil
	})
	return applied, MapError(err, OpWrite, "work order")
}

// ---- notifications ---------------------------------------------------------------------------------------

// Sent implements workorder.NotifyStore.
func (s *WorkOrderStore) Sent(ctx context.Context, ids []string) (map[string]map[workorder.NotificationKey]bool, error) {
	out := map[string]map[workorder.NotificationKey]bool{}
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return collect(ctx, t, "work order notifications", `SELECT work_order_id, coalesce(host, ''), coalesce(repo, ''), kind, recipient
 FROM afs.work_order_notifications WHERE work_order_id = ANY($1)`, []any{ids}, func(rows pgx.Rows) error {
			var id string
			var k workorder.NotificationKey
			if err := rows.Scan(&id, &k.Host, &k.Repo, &k.Kind, &k.Recipient); err != nil {
				return err
			}
			if out[id] == nil {
				out[id] = map[workorder.NotificationKey]bool{}
			}
			out[id][k] = true
			return nil
		})
	})
	return out, err
}

// NotificationChannel implements workorder.NotifyStore.
func (s *WorkOrderStore) NotificationChannel(ctx context.Context) (string, error) {
	var id string
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		err = t.QueryRow(ctx, `SELECT c.id FROM afs.service_settings s JOIN afs.notification_channels c ON c.id = s.value #>> '{}'
 WHERE s.key = 'work_orders.notification_channel_id' AND jsonb_typeof(s.value) = 'string'
   AND c.team_id IS NULL AND c.kind = 'email' AND c.enabled`).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return id, MapError(err, OpRead, "notification channel")
}

const insertNotificationSQL = `INSERT INTO afs.work_order_notifications (work_order_id, host, repo, kind, recipient, channel_id)
 VALUES ($1, nullif($2, ''), nullif($3, ''), $4, $5, nullif($6, '')) ON CONFLICT DO NOTHING`

var notificationEvents = map[string]domain.EventType{
	workorder.KindAssigned:    domain.EventWorkOrderAssigned,
	workorder.KindPROpened:    domain.EventWorkOrderPrOpened,
	workorder.KindMerged:      domain.EventWorkOrderMerged,
	workorder.KindSLAWarning:  domain.EventWorkOrderSLAWarning,
	workorder.KindSLABreached: domain.EventWorkOrderSLABreached,
	workorder.KindCompleted:   domain.EventWorkOrderCompleted,
}

// RecordTeam implements workorder.NotifyStore.
func (s *WorkOrderStore) RecordTeam(ctx context.Context, n workorder.Notification) (bool, error) {
	return s.record(ctx, n, n.Recipient)
}

// RecordEvent implements workorder.NotifyStore.
func (s *WorkOrderStore) RecordEvent(ctx context.Context, n workorder.Notification) (bool, error) {
	return s.record(ctx, n, n.WorkOrderTeamID)
}

// RecordContact implements workorder.NotifyStore.
func (s *WorkOrderStore) RecordContact(ctx context.Context, n workorder.Notification) (bool, error) {
	return s.record(ctx, n, "")
}

// record inserts the notification row and, when eventTeam is set or the recipient is the sentinel, the event the
// row stands for, in one transaction. The event's team is eventTeam: the recipient team for a team notification
// (RC-15 routes by it), the remediation team (or none) for the work order's own event.
func (s *WorkOrderStore) record(ctx context.Context, n workorder.Notification, eventTeam string) (bool, error) {
	inserted := false
	emit := n.IsTeam() || n.Recipient == workorder.SentinelRecipient
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		inserted = false
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		tag, err := t.Exec(ctx, insertNotificationSQL, n.WorkOrderID, n.Host, n.Repo, n.Kind, n.Recipient, n.ChannelID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		inserted = true
		if !emit {
			return nil
		}
		typ, ok := notificationEvents[n.Kind]
		if !ok {
			return fmt.Errorf("no event for notification kind %q", n.Kind)
		}
		data := make(map[string]any, len(n.Data)+1)
		for k, v := range n.Data {
			data[k] = v
		}
		data["notification"] = true
		return s.publish(ctx, tx, typ, eventTeam, n.WorkOrderID, s.now(), data)
	})
	return inserted, MapError(err, OpWrite, "work order notification")
}

func (s *WorkOrderStore) publish(ctx context.Context, tx port.Tx, typ domain.EventType, teamID, workOrderID string, at time.Time, data map[string]any) error {
	id, err := s.ids.NewID(eventIDPrefix)
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, id.String(), at.UTC(), tx.Session().Actor, teamID, kindWorkOrders, workOrderID, data)
	if err != nil {
		return err
	}
	return s.outbox.Publish(ctx, tx, ev)
}
