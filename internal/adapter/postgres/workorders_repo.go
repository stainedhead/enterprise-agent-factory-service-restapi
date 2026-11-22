package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/workorders"
)

// WorkOrderRepo implements workorders.Repository on afs.work_orders, work_order_repos and
// work_order_notifications (000012, 000016). Row-level security does the scoping: security operators
// and service admins see and write every work order; a team sees the work orders assigned to it or
// touching a repository it owns, and the assigned team updates only its repositories' progress
// columns (the work_order_repo_progress_guard trigger).
type WorkOrderRepo struct{}

// NewWorkOrderRepo returns a WorkOrderRepo.
func NewWorkOrderRepo() *WorkOrderRepo { return &WorkOrderRepo{} }

var _ workorders.Repository = (*WorkOrderRepo)(nil)

// Template implements workorders.Repository.
func (*WorkOrderRepo) Template(ctx context.Context, tx port.Tx, id string) (workorders.TemplateRef, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return workorders.TemplateRef{}, err
	}
	var t workorders.TemplateRef
	var budget string
	err = pt.QueryRow(ctx, `SELECT id, name, enabled, budget_usd::text FROM afs.swarm_templates WHERE id = $1`, id).Scan(&t.ID, &t.Name, &t.Enabled, &budget)
	if err != nil {
		return t, MapError(err, OpRead, "swarm template")
	}
	t.Budget, err = domain.ParseMoney("budget_usd", budget)
	return t, err
}

// ExistingTeams implements workorders.Repository through afs.team_directory (000016), which lists the
// teams that are not deleted for service admins and security operators.
func (*WorkOrderRepo) ExistingTeams(ctx context.Context, tx port.Tx, ids []string) (map[string]bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id FROM afs.team_directory() WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, MapError(err, OpRead, "team")
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, MapError(err, OpRead, "team")
		}
		out[id] = true
	}
	return out, MapError(rows.Err(), OpRead, "team")
}

// TeamVisible implements workorders.Repository.
func (*WorkOrderRepo) TeamVisible(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.teams WHERE id = $1 AND state <> 'deleted')`, teamID).Scan(&ok)
	return ok, MapError(err, OpRead, "team")
}

// ResolveOwners implements workorders.Repository through afs.resolve_repo_owners (000014).
func (*WorkOrderRepo) ResolveOwners(ctx context.Context, tx port.Tx, host, repo string) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT afs.resolve_repo_owners($1, $2)`, host, repo)
	if err != nil {
		return nil, MapError(err, OpRead, "repository owner")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, MapError(err, OpRead, "repository owner")
		}
		out = append(out, id)
	}
	return out, MapError(rows.Err(), OpRead, "repository owner")
}

// OpenCVE implements workorders.Repository.
func (*WorkOrderRepo) OpenCVE(ctx context.Context, tx port.Tx, cve string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.work_orders WHERE kind = 'cve_remediation' AND external_ref = $1
  AND status NOT IN ('done', 'failed', 'cancelled'))`, cve).Scan(&ok)
	return ok, MapError(err, OpRead, "work order")
}

// Insert implements workorders.Repository. The open-CVE unique index is the backstop of the use
// case's check: a concurrent duplicate is a Conflict.
func (r *WorkOrderRepo) Insert(ctx context.Context, tx port.Tx, w domain.WorkOrder, repos []domain.WorkOrderRepo) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.work_orders (id, kind, external_ref, title, sla_due_at, severity, source, template_id, budget_usd, status)
VALUES ($1, $2, nullif($3, ''), $4, $5, $6, $7, $8, $9::numeric, 'open')`,
		w.ID, string(w.Kind), w.CVEID, w.Title, w.SLADueAt, string(w.Severity), string(w.Source), w.TemplateID, w.Budget.String())
	if err != nil {
		var de *domain.Error
		mapped := MapError(err, OpWrite, "work order")
		if errors.As(mapped, &de) && de.Kind == domain.KindConflict {
			return domain.Conflict("open_work_order_exists", "an open work order already exists for "+w.CVEID, map[string]any{"cveId": w.CVEID})
		}
		return mapped
	}
	return r.AddRepos(ctx, tx, w.ID, repos)
}

const workOrderSelect = `SELECT w.id, w.kind, coalesce(w.external_ref, ''), w.severity, w.title, w.source, coalesce(w.template_id, ''),
  coalesce(w.budget_usd, t.budget_usd, 0)::text, w.sla_due_at, w.status, coalesce(w.team_id, ''), w.assigned_at, w.completed_at,
  coalesce(w.payload ->> 'cancelReason', ''), w.version, w.created_at, w.created_by, w.updated_at,
  p.repos, p.prs_open, p.prs_merged, p.not_affected, p.failed_or_skipped, p.remaining
FROM afs.work_orders w
JOIN afs.work_order_progress p ON p.work_order_id = w.id
LEFT JOIN afs.swarm_templates t ON t.id = w.template_id`

func scanWorkOrder(row pgx.Row) (domain.WorkOrder, error) {
	var w domain.WorkOrder
	var kind, sev, src, status, budget string
	var sla *time.Time
	var v int64
	var pr domain.WorkOrderProgress
	if err := row.Scan(&w.ID, &kind, &w.CVEID, &sev, &w.Title, &src, &w.TemplateID, &budget, &sla, &status, &w.TeamID,
		&w.AssignedAt, &w.CompletedAt, &w.CancelReason, &v, &w.CreatedAt, &w.CreatedBy, &w.UpdatedAt,
		&pr.Repos, &pr.PRsOpen, &pr.PRsMerged, &pr.NotAffected, &pr.FailedOrSkipped, &pr.Remaining); err != nil {
		return w, err
	}
	w.Kind, w.Severity, w.Source, w.Status = domain.WorkOrderKind(kind), domain.Severity(sev), domain.WorkOrderSource(src), domain.WorkOrderStatus(status)
	w.Version, w.Progress = domain.Version(v), pr
	if sla != nil {
		w.SLADueAt = sla.UTC()
	}
	var err error
	w.Budget, err = domain.ParseMoney("budget_usd", budget)
	return w, err
}

// Get implements workorders.Repository. The row is locked first (a lock cannot be taken through the
// progress view's GROUP BY).
func (*WorkOrderRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.WorkOrder, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.WorkOrder{}, err
	}
	if forUpdate {
		var one int
		if err := pt.QueryRow(ctx, `SELECT 1 FROM afs.work_orders WHERE id = $1 FOR UPDATE`, id).Scan(&one); err != nil {
			return domain.WorkOrder{}, MapError(err, OpRead, "work order")
		}
	}
	w, err := scanWorkOrder(pt.QueryRow(ctx, workOrderSelect+` WHERE w.id = $1`, id))
	return w, MapError(err, OpRead, "work order")
}

// List implements workorders.Repository.
func (*WorkOrderRepo) List(ctx context.Context, tx port.Tx, f workorders.Filter, after string, limit int) ([]domain.WorkOrder, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, workOrderSelect+` WHERE ($1 = '' OR w.id < $1) AND ($2 = '' OR w.status = $2) AND ($3 = '' OR w.severity = $3)
  AND ($4 = '' OR p.sla_state = $4)
  AND ($5 = '' OR w.team_id = $5 OR EXISTS (SELECT 1 FROM afs.work_order_repos r WHERE r.work_order_id = w.id AND r.owning_team_id = $5))
ORDER BY w.id DESC LIMIT $6`, after, f.Status, f.Severity, f.SLAState, f.TeamID, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "work order")
	}
	defer rows.Close()
	out := []domain.WorkOrder{}
	for rows.Next() {
		w, err := scanWorkOrder(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "work order")
		}
		out = append(out, w)
	}
	return out, MapError(rows.Err(), OpRead, "work order")
}

// Update implements workorders.Repository. A budget the work order never set (it follows its template)
// stays unset when the update leaves it equal to the template's.
func (r *WorkOrderRepo) Update(ctx context.Context, tx port.Tx, w domain.WorkOrder, expect domain.Version) (domain.WorkOrder, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.WorkOrder{}, err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.work_orders SET title = $2, sla_due_at = $3, status = $4, completed_at = $5,
  budget_usd = CASE WHEN budget_usd IS NULL AND $6::numeric = (SELECT s.budget_usd FROM afs.swarm_templates s WHERE s.id = template_id)
                    THEN NULL ELSE $6::numeric END,
  payload = CASE WHEN $7::text <> '' THEN payload || jsonb_build_object('cancelReason', $7::text) ELSE payload END
WHERE id = $1 AND version = $8`,
		w.ID, w.Title, w.SLADueAt, string(w.Status), w.CompletedAt, w.Budget.String(), w.CancelReason, int64(expect))
	if err != nil {
		return domain.WorkOrder{}, MapError(err, OpWrite, "work order")
	}
	if tag.RowsAffected() == 0 {
		return domain.WorkOrder{}, catalogMissOrStale(ctx, pt, "work order", `SELECT 1 FROM afs.work_orders WHERE id = $1`, w.ID)
	}
	return r.Get(ctx, tx, w.ID, false)
}

const repoSelect = `SELECT r.host, r.repo, coalesce(r.owning_team_id, ''), coalesce(r.owning_contact, ''), r.status, coalesce(r.pr_url, ''),
  coalesce(r.pr_number, 0), coalesce(r.detail, ''), r.merged_at, r.version, r.updated_at FROM afs.work_order_repos r`

func scanRepo(id string, row pgx.Row) (domain.WorkOrderRepo, error) {
	r := domain.WorkOrderRepo{WorkOrderID: id}
	var status string
	var v int64
	err := row.Scan(&r.Host, &r.Repo, &r.OwningTeamID, &r.OwningContact, &status, &r.PRURL, &r.PRNumber, &r.Detail, &r.MergedAt, &v, &r.UpdatedAt)
	r.Status, r.Version = domain.RepoStatus(status), domain.Version(v)
	return r, err
}

// Repos implements workorders.Repository.
func (*WorkOrderRepo) Repos(ctx context.Context, tx port.Tx, id, after string, limit int) ([]domain.WorkOrderRepo, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, repoSelect+` WHERE r.work_order_id = $1 AND lower(r.host || '/' || r.repo) > $2
ORDER BY lower(r.host || '/' || r.repo) LIMIT $3`, id, strings.ToLower(after), limit)
	if err != nil {
		return nil, MapError(err, OpRead, "work order repository")
	}
	defer rows.Close()
	out := []domain.WorkOrderRepo{}
	idx := map[string]int{}
	for rows.Next() {
		r, err := scanRepo(id, rows)
		if err != nil {
			return nil, MapError(err, OpRead, "work order repository")
		}
		idx[r.Key()] = len(out)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, MapError(err, OpRead, "work order repository")
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	nrows, err := pt.Query(ctx, `SELECT host, repo, kind, recipient, sent_at FROM afs.work_order_notifications
WHERE work_order_id = $1 AND host IS NOT NULL ORDER BY sent_at, id`, id)
	if err != nil {
		return nil, MapError(err, OpRead, "work order notification")
	}
	defer nrows.Close()
	for nrows.Next() {
		var host, repo string
		var n domain.WorkOrderNotification
		if err := nrows.Scan(&host, &repo, &n.Kind, &n.Recipient, &n.SentAt); err != nil {
			return nil, MapError(err, OpRead, "work order notification")
		}
		if i, ok := idx[strings.ToLower(host+"/"+repo)]; ok {
			out[i].Notifications = append(out[i].Notifications, n)
		}
	}
	return out, MapError(nrows.Err(), OpRead, "work order notification")
}

// OwnerTeams implements workorders.Repository.
func (*WorkOrderRepo) OwnerTeams(ctx context.Context, tx port.Tx, id string) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT DISTINCT owning_team_id FROM afs.work_order_repos WHERE work_order_id = $1 AND owning_team_id IS NOT NULL`, id)
	if err != nil {
		return nil, MapError(err, OpRead, "work order repository")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, MapError(err, OpRead, "work order repository")
		}
		out = append(out, t)
	}
	return out, MapError(rows.Err(), OpRead, "work order repository")
}

// Repo implements workorders.Repository; host and repository names match case-insensitively.
func (*WorkOrderRepo) Repo(ctx context.Context, tx port.Tx, id, host, repo string, forUpdate bool) (domain.WorkOrderRepo, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.WorkOrderRepo{}, err
	}
	q := repoSelect + ` WHERE r.work_order_id = $1 AND lower(r.host) = lower($2) AND lower(r.repo) = lower($3)`
	if forUpdate {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanRepo(id, pt.QueryRow(ctx, q, id, host, repo))
	return r, MapError(err, OpRead, "work order repository")
}

// AddRepos implements workorders.Repository.
func (*WorkOrderRepo) AddRepos(ctx context.Context, tx port.Tx, id string, repos []domain.WorkOrderRepo) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if _, err := pt.Exec(ctx, `INSERT INTO afs.work_order_repos (work_order_id, host, repo, owning_team_id, owning_contact)
VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''))`, id, r.Host, r.Repo, r.OwningTeamID, r.OwningContact); err != nil {
			mapped := MapError(err, OpWrite, "work order repository")
			var de *domain.Error
			if errors.As(mapped, &de) && de.Kind == domain.KindConflict {
				return domain.Conflict("duplicate", "work order already has repository "+r.Host+"/"+r.Repo, nil)
			}
			return mapped
		}
	}
	return nil
}

// UpdateRepo implements workorders.Repository.
func (r *WorkOrderRepo) UpdateRepo(ctx context.Context, tx port.Tx, id string, n domain.WorkOrderRepo, expect domain.Version) (domain.WorkOrderRepo, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.WorkOrderRepo{}, err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.work_order_repos SET status = $4, pr_url = nullif($5, ''), pr_number = nullif($6, 0),
  detail = nullif($7, ''), merged_at = $8
WHERE work_order_id = $1 AND host = $2 AND repo = $3 AND version = $9`,
		id, n.Host, n.Repo, string(n.Status), n.PRURL, n.PRNumber, n.Detail, n.MergedAt, int64(expect))
	if err != nil {
		return domain.WorkOrderRepo{}, MapError(err, OpWrite, "work order repository")
	}
	if tag.RowsAffected() == 0 {
		return domain.WorkOrderRepo{}, catalogMissOrStale(ctx, pt, "work order repository",
			`SELECT 1 FROM afs.work_order_repos WHERE work_order_id = $1 AND host = $2 AND repo = $3`, id, n.Host, n.Repo)
	}
	out, err := r.Repo(ctx, tx, id, n.Host, n.Repo, false)
	return out, err
}
