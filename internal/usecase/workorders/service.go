// Package workorders implements the work order operations of API contract §4.10a (M6-C, RA-9, D38):
// create (with owner resolution through the teams' AFS_OWNED_REPOS configuration), list, get, update,
// cancel, list repositories and report a repository's progress. Creating, updating and cancelling are
// for security operators and service admins; the assigned team (or its workload principals) reports
// progress on its own work order's repositories. Row-level security hides work orders a team caller
// may not see, so an unrelated team gets NotFound (404), never a hint that the work order exists.
// The reconciler, not the API, assigns the team, tracks pull requests and finishes the work order.
package workorders

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "work_orders"

// TemplateRef is what a work order needs of its swarm template.
type TemplateRef struct {
	ID      string
	Name    string
	Enabled bool
	Budget  domain.Money
}

// Filter narrows ListWorkOrders. TeamID matches the assigned team and the teams that own one of the
// work order's repositories.
type Filter struct {
	Status   string
	Severity string
	SLAState string
	TeamID   string
}

// Repository is the work order store (work_orders, work_order_repos, work_order_notifications).
type Repository interface {
	// Template returns the swarm template; NotFound when absent.
	Template(ctx context.Context, tx port.Tx, id string) (TemplateRef, error)
	// ExistingTeams returns which of ids are teams that are not deleted, by the team directory (a
	// security operator cannot read teams through row-level security).
	ExistingTeams(ctx context.Context, tx port.Tx, ids []string) (map[string]bool, error)
	// TeamVisible reports whether the caller can see the team (afs.teams under row-level security).
	TeamVisible(ctx context.Context, tx port.Tx, teamID string) (bool, error)
	// ResolveOwners is afs.resolve_repo_owners: the teams at the best match for host and repo.
	ResolveOwners(ctx context.Context, tx port.Tx, host, repo string) ([]string, error)
	// OpenCVE reports whether an open cve_remediation work order exists for the CVE.
	OpenCVE(ctx context.Context, tx port.Tx, cve string) (bool, error)
	// Insert stores the work order and its repositories.
	Insert(ctx context.Context, tx port.Tx, w domain.WorkOrder, repos []domain.WorkOrderRepo) error
	// Get returns the work order with its progress counts; NotFound when absent or not visible.
	Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.WorkOrder, error)
	// List returns work orders newest first (id below after) that pass the filter.
	List(ctx context.Context, tx port.Tx, f Filter, after string, limit int) ([]domain.WorkOrder, error)
	// Update stores the work order's mutable fields when its version is expect.
	Update(ctx context.Context, tx port.Tx, w domain.WorkOrder, expect domain.Version) (domain.WorkOrder, error)
	// Repos returns repositories with notifications, in host/repo order, after the key host/repo.
	Repos(ctx context.Context, tx port.Tx, id, after string, limit int) ([]domain.WorkOrderRepo, error)
	// OwnerTeams returns the distinct teams that own a repository of the work order.
	OwnerTeams(ctx context.Context, tx port.Tx, id string) ([]string, error)
	// Repo returns one repository; NotFound when absent or not visible.
	Repo(ctx context.Context, tx port.Tx, id, host, repo string, forUpdate bool) (domain.WorkOrderRepo, error)
	// AddRepos stores more repositories of the work order.
	AddRepos(ctx context.Context, tx port.Tx, id string, repos []domain.WorkOrderRepo) error
	// UpdateRepo stores the repository's progress columns when its version is expect.
	UpdateRepo(ctx context.Context, tx port.Tx, id string, r domain.WorkOrderRepo, expect domain.Version) (domain.WorkOrderRepo, error)
}

// Service is the work order use case set.
type Service struct {
	acting.Deps
	Repo Repository
	// Settings supplies work_orders.sla_warning_hours; nil keeps the default.
	Settings port.SettingsReader
}

// Result is a work order with the replayed response of an idempotent call.
type Result struct {
	WorkOrder domain.WorkOrder
	Replay    *port.StoredResponse
}

func (s *Service) warnHours(ctx context.Context) int {
	if s.Settings == nil {
		return domain.DefaultSLAWarningHours
	}
	raw, err := s.Settings.Setting(ctx, domain.SettingWorkOrdersSLAWarningHours)
	var n int
	if err != nil || json.Unmarshal(raw, &n) != nil || n < 1 {
		return domain.DefaultSLAWarningHours
	}
	return n
}

// withSLA fills the SLA state the response shows.
func (s *Service) withSLA(ctx context.Context, w ...*domain.WorkOrder) {
	hours := s.warnHours(ctx)
	for _, x := range w {
		x.SLAState = x.SLAStateAt(s.Now(), hours)
	}
}

// route checks the permission at route level, before any work order is loaded.
func (s *Service) route(ctx context.Context, c acting.Caller, action string, perms ...authz.Permission) error {
	if !authz.Check(c.Principal, authz.Requirement{AnyOf: perms}, authz.Resource{Scope: authz.ScopeAny}).Allowed() {
		return s.Denied(ctx, c, action, "", perms[0])
	}
	return nil
}

// onWorkOrder checks the permission against the loaded work order: a caller that holds the permission
// elsewhere but cannot see this work order gets NotFound.
func (s *Service) onWorkOrder(ctx context.Context, c acting.Caller, tx port.Tx, action string, w domain.WorkOrder, perms ...authz.Permission) error {
	owners, err := s.Repo.OwnerTeams(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	ref := &authz.WorkOrderRef{AssignedTeamID: w.TeamID, RepoOwnerTeamIDs: owners}
	d := authz.Check(c.Principal, authz.Requirement{AnyOf: perms}, authz.Resource{Scope: authz.ScopeAny, WorkOrder: ref})
	switch d.Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("work order")
	}
	return s.Denied(ctx, c, action, w.TeamID, perms[0])
}

func (s *Service) emit(ctx context.Context, tx port.Tx, c acting.Caller, typ domain.EventType, w domain.WorkOrder, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	data["workOrderId"] = w.ID
	return s.Emit(ctx, tx, c, typ, w.TeamID, kind, w.ID, data)
}

// resolveOwners fills the owners of repositories that name none (D38) and checks explicit teams. Both
// kinds of failure refuse the whole request: field errors for an unknown explicit team, and the
// ambiguousRepos / unownedRepos details for owners that cannot be resolved.
func (s *Service) resolveOwners(ctx context.Context, tx port.Tx, in []domain.RepoInput) ([]domain.WorkOrderRepo, error) {
	var explicit []string
	for _, r := range in {
		if r.OwningTeamID != "" && !slices.Contains(explicit, r.OwningTeamID) {
			explicit = append(explicit, r.OwningTeamID)
		}
	}
	known := map[string]bool{}
	if len(explicit) > 0 {
		var err error
		if known, err = s.Repo.ExistingTeams(ctx, tx, explicit); err != nil {
			return nil, err
		}
	}
	var fe []domain.FieldError
	var ambiguous []domain.AmbiguousRepo
	var unowned []string
	out := make([]domain.WorkOrderRepo, 0, len(in))
	for i, r := range in {
		repo := domain.WorkOrderRepo{Host: r.Host, Repo: r.Repo, OwningTeamID: r.OwningTeamID, OwningContact: r.OwningContact,
			OwnerSource: domain.OwnerSourceExplicit, Status: domain.RepoStatusPending, Version: 1}
		switch {
		case r.OwningTeamID != "" && !known[r.OwningTeamID]:
			fe = append(fe, domain.FieldError{Field: "repos[" + itoa(i) + "].owningTeamId", Reason: "is not a known team (unknown or deleted)"})
		case r.HasOwner():
		default:
			ids, err := s.Repo.ResolveOwners(ctx, tx, r.Host, r.Repo)
			if err != nil {
				return nil, err
			}
			name := r.Host + "/" + r.Repo
			switch len(ids) {
			case 0:
				unowned = append(unowned, name)
			case 1:
				repo.OwningTeamID, repo.OwnerSource = ids[0], domain.OwnerSourceTeamConfig
			default:
				slices.Sort(ids)
				ambiguous = append(ambiguous, domain.AmbiguousRepo{Repo: name, TeamIDs: ids})
			}
		}
		out = append(out, repo)
	}
	if len(fe) > 0 {
		return nil, domain.Validation(fe...)
	}
	if len(ambiguous) > 0 || len(unowned) > 0 {
		return nil, domain.OwnerResolutionError(ambiguous, unowned)
	}
	return out, nil
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// Create implements CreateWorkOrder (work-orders:write). Nothing is written when the template is
// disabled, a repository's owner cannot be settled, or an open work order exists for the CVE.
func (s *Service) Create(ctx context.Context, c acting.Caller, in domain.NewWorkOrderParams, repos []domain.RepoInput, idem *acting.Idem) (Result, error) {
	if err := s.route(ctx, c, "CreateWorkOrder", authz.WorkOrdersWrite); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		tpl, err := s.Repo.Template(ctx, tx, in.TemplateID)
		switch {
		case domain.KindOf(err) == domain.KindNotFound:
			return domain.Validation(domain.FieldError{Field: "templateId", Reason: "is not a known swarm template"})
		case err != nil:
			return err
		case !tpl.Enabled:
			return domain.Validation(domain.FieldError{Field: "templateId", Reason: "swarm template " + tpl.Name + " is disabled"})
		}
		id, err := s.IDs.NewID(domain.PrefixWorkOrder)
		if err != nil {
			return err
		}
		w, err := domain.NewWorkOrder(id.String(), in, tpl.Budget, s.Now())
		if err != nil {
			return err
		}
		norm, err := domain.NormalizeRepoInputs("repos", repos, true)
		if err != nil {
			return err
		}
		owned, err := s.resolveOwners(ctx, tx, norm)
		if err != nil {
			return err
		}
		if w.Kind == domain.WorkOrderKindCveRemediation {
			open, err := s.Repo.OpenCVE(ctx, tx, w.CVEID)
			if err != nil {
				return err
			}
			if open {
				return domain.Conflict("open_work_order_exists", "an open work order already exists for "+w.CVEID, map[string]any{"cveId": w.CVEID})
			}
		}
		if err := s.Repo.Insert(ctx, tx, w, owned); err != nil {
			return err
		}
		if res.WorkOrder, err = s.Repo.Get(ctx, tx, w.ID, false); err != nil {
			return err
		}
		res.WorkOrder.Repos = owned
		s.withSLA(ctx, &res.WorkOrder)
		if err := s.Audit.Write(ctx, tx, c.Record("CreateWorkOrder", kind, w.ID, "", nil, res.WorkOrder.Audit())); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventWorkOrderCreated, res.WorkOrder, map[string]any{"kind": string(w.Kind), "cveId": w.CVEID,
			"severity": string(w.Severity), "templateId": w.TemplateID, "repos": len(owned)}); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.WorkOrder)
	})
	return res, err
}

// List implements ListWorkOrders (work-orders:read). Row-level security limits a team caller to the
// work orders assigned to its teams or touching repositories they own.
func (s *Service) List(ctx context.Context, c acting.Caller, f Filter, p acting.Page) ([]domain.WorkOrder, string, error) {
	if err := s.route(ctx, c, "ListWorkOrders", authz.WorkOrdersRead); err != nil {
		return nil, "", err
	}
	return s.list(ctx, c, f, p)
}

// ListForTeam implements ListTeamWorkOrders: the work orders of one team (team-scoped route; the
// route already refused security operators and callers not bound to the team).
func (s *Service) ListForTeam(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.WorkOrder, string, error) {
	if !authz.Authorize(c.Principal, authz.WorkOrdersRead, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Allowed() {
		return nil, "", s.Denied(ctx, c, "ListTeamWorkOrders", teamID, authz.WorkOrdersRead)
	}
	rows, next, err := s.listIn(ctx, c, Filter{TeamID: teamID}, p, func(ctx context.Context, tx port.Tx) error {
		ok, err := s.Repo.TeamVisible(ctx, tx, teamID)
		if err == nil && !ok {
			return domain.NotFound("team")
		}
		return err
	})
	return rows, next, err
}

func (s *Service) list(ctx context.Context, c acting.Caller, f Filter, p acting.Page) ([]domain.WorkOrder, string, error) {
	return s.listIn(ctx, c, f, p, nil)
}

func (s *Service) listIn(ctx context.Context, c acting.Caller, f Filter, p acting.Page, pre func(context.Context, port.Tx) error) ([]domain.WorkOrder, string, error) {
	var bad []domain.FieldError
	if f.Status != "" && !domain.WorkOrderStatus(f.Status).Valid() {
		bad = append(bad, domain.FieldError{Field: "status", Reason: "is not a work order status"})
	}
	if f.Severity != "" && !domain.Severity(f.Severity).Valid() {
		bad = append(bad, domain.FieldError{Field: "severity", Reason: "must be critical, high, medium or low"})
	}
	if f.SLAState != "" && !domain.SLAState(f.SLAState).Valid() {
		bad = append(bad, domain.FieldError{Field: "slaState", Reason: "must be on_track, due_soon, breached, met or missed"})
	}
	if len(bad) > 0 {
		return nil, "", domain.Validation(bad...)
	}
	p = p.Normalized()
	var rows []domain.WorkOrder
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if pre != nil {
			if err := pre(ctx, tx); err != nil {
				return err
			}
		}
		var err error
		rows, err = s.Repo.List(ctx, tx, f, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(w domain.WorkOrder) string { return w.ID })
	hours := s.warnHours(ctx)
	for i := range page {
		page[i].SLAState = page[i].SLAStateAt(s.Now(), hours)
	}
	return page, next, nil
}

// Get implements GetWorkOrder: the work order with its repositories, progress, SLA state and the
// notifications sent.
func (s *Service) Get(ctx context.Context, c acting.Caller, id string) (domain.WorkOrder, error) {
	if err := s.route(ctx, c, "GetWorkOrder", authz.WorkOrdersRead); err != nil {
		return domain.WorkOrder{}, err
	}
	var out domain.WorkOrder
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		w, err := s.Repo.Get(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := s.onWorkOrder(ctx, c, tx, "GetWorkOrder", w, authz.WorkOrdersRead); err != nil {
			return err
		}
		if w.Repos, err = s.Repo.Repos(ctx, tx, id, "", domain.MaxWorkOrderRepos+1); err != nil {
			return err
		}
		out = w
		return nil
	})
	if err == nil {
		s.withSLA(ctx, &out)
	}
	return out, err
}

// ListRepos implements ListWorkOrderRepos.
func (s *Service) ListRepos(ctx context.Context, c acting.Caller, id string, p acting.Page) ([]domain.WorkOrderRepo, string, error) {
	if err := s.route(ctx, c, "ListWorkOrderRepos", authz.WorkOrdersRead); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.WorkOrderRepo
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		w, err := s.Repo.Get(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := s.onWorkOrder(ctx, c, tx, "ListWorkOrderRepos", w, authz.WorkOrdersRead); err != nil {
			return err
		}
		rows, err = s.Repo.Repos(ctx, tx, id, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r domain.WorkOrderRepo) string { return strings.ToLower(r.Host + "/" + r.Repo) })
	return page, next, nil
}

// Update implements UpdateWorkOrder (work-orders:write, If-Match): title, SLA due time, budget cap
// and added repositories (their owners are resolved as at create). A body that changes nothing is a
// ValidationError. A budget change reaches only a team created afterwards.
func (s *Service) Update(ctx context.Context, c acting.Caller, id string, expect domain.Version, patch domain.WorkOrderPatch, addRepos []domain.RepoInput) (domain.WorkOrder, error) {
	if err := s.route(ctx, c, "UpdateWorkOrder", authz.WorkOrdersWrite); err != nil {
		return domain.WorkOrder{}, err
	}
	if patch.Empty() && len(addRepos) == 0 {
		return domain.WorkOrder{}, domain.Validation(domain.FieldError{Field: "body", Reason: "names no field to change"})
	}
	var out domain.WorkOrder
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(patch, s.Now())
		if err != nil {
			return err
		}
		var added []domain.WorkOrderRepo
		if len(addRepos) > 0 {
			if err := cur.CanAddRepos(); err != nil {
				return err
			}
			norm, err := domain.NormalizeRepoInputs("addRepos", addRepos, false)
			if err != nil {
				return err
			}
			if added, err = s.resolveOwners(ctx, tx, norm); err != nil {
				return err
			}
			if err := s.checkRepoLimit(ctx, tx, id, len(added)); err != nil {
				return err
			}
			if err := s.Repo.AddRepos(ctx, tx, id, added); err != nil {
				return err
			}
		}
		if out, err = s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		if out.Repos, err = s.Repo.Repos(ctx, tx, id, "", domain.MaxWorkOrderRepos+1); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("UpdateWorkOrder", kind, id, cur.TeamID, cur.Audit(), out.Audit())); err != nil {
			return err
		}
		if len(added) == 0 {
			return nil
		}
		names := make([]string, len(added))
		for i, r := range added {
			names[i] = r.Host + "/" + r.Repo
		}
		return s.emit(ctx, tx, c, domain.EventWorkOrderRepoUpdated, out, map[string]any{"added": names})
	})
	if err == nil {
		s.withSLA(ctx, &out)
	}
	return out, err
}

func (s *Service) checkRepoLimit(ctx context.Context, tx port.Tx, id string, adding int) error {
	have, err := s.Repo.Repos(ctx, tx, id, "", domain.MaxWorkOrderRepos+1)
	if err != nil {
		return err
	}
	if len(have)+adding > domain.MaxWorkOrderRepos {
		return domain.Validation(domain.FieldError{Field: "addRepos", Reason: "a work order has at most " + itoa(domain.MaxWorkOrderRepos) + " repositories"})
	}
	return nil
}

// Cancel implements CancelWorkOrder (work-orders:write). The reconciler tears the team down.
func (s *Service) Cancel(ctx context.Context, c acting.Caller, id, reason string, idem *acting.Idem) (Result, error) {
	if err := s.route(ctx, c, "CancelWorkOrder", authz.WorkOrdersWrite); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		cur, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		next, err := cur.Cancel(reason, s.Now())
		if err != nil {
			return err
		}
		if res.WorkOrder, err = s.Repo.Update(ctx, tx, next, cur.Version); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CancelWorkOrder", kind, id, cur.TeamID, cur.Audit(), res.WorkOrder.Audit())); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventWorkOrderCancelled, res.WorkOrder, map[string]any{"reason": next.CancelReason}); err != nil {
			return err
		}
		s.withSLA(ctx, &res.WorkOrder)
		return idem.Complete(ctx, s.Idem, tx, res.WorkOrder)
	})
	return res, err
}

// UpdateRepo implements UpdateWorkOrderRepo: work-orders:report on the assigned team (a team admin or
// a workload principal of that team) or work-orders:write. It changes only the repository's progress
// columns and never the work order's status (the reconciler derives that). expect is the If-Match version; a
// workload principal, which has no read of it (CLI-CD-1), may pass 0 for the current version.
func (s *Service) UpdateRepo(ctx context.Context, c acting.Caller, id, host, repo string, expect domain.Version, rep domain.RepoReport) (domain.WorkOrderRepo, error) {
	perms := []authz.Permission{authz.WorkOrdersReport, authz.WorkOrdersWrite}
	if err := s.route(ctx, c, "UpdateWorkOrderRepo", perms...); err != nil {
		return domain.WorkOrderRepo{}, err
	}
	var out domain.WorkOrderRepo
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		w, err := s.Repo.Get(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := s.onWorkOrder(ctx, c, tx, "UpdateWorkOrderRepo", w, perms...); err != nil {
			return err
		}
		if !w.Open() {
			return domain.Conflict("work_order_closed", "work order is "+string(w.Status)+" and cannot change", map[string]any{"status": string(w.Status)})
		}
		cur, err := s.Repo.Repo(ctx, tx, id, host, repo, true)
		if err != nil {
			return err
		}
		if expect == 0 && c.Principal.Workload != nil {
			expect = cur.Version // CLI-CD-1: a workload cannot read the version, so it may send none
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(rep, s.Now())
		if err != nil {
			return err
		}
		if out, err = s.Repo.UpdateRepo(ctx, tx, id, next, expect); err != nil {
			return err
		}
		rec := c.Record("UpdateWorkOrderRepo", kind, id, w.TeamID, cur.Audit(), out.Audit())
		if wl := c.Principal.Workload; wl != nil {
			rec.PoolID, rec.WorkerPod = wl.PoolID, wl.WorkerPod
		}
		if err := s.Audit.Write(ctx, tx, rec); err != nil {
			return err
		}
		data := map[string]any{"host": out.Host, "repo": out.Repo, "status": string(out.Status), "previousStatus": string(cur.Status)}
		if err := s.emit(ctx, tx, c, domain.EventWorkOrderRepoUpdated, w, data); err != nil {
			return err
		}
		if out.Status == cur.Status {
			return nil
		}
		switch out.Status {
		case domain.RepoStatusPrOpened:
			data["prUrl"], data["prNumber"] = out.PRURL, out.PRNumber
			return s.emit(ctx, tx, c, domain.EventWorkOrderPrOpened, w, data)
		case domain.RepoStatusMerged:
			data["prUrl"] = out.PRURL
			return s.emit(ctx, tx, c, domain.EventWorkOrderMerged, w, data)
		}
		return nil
	})
	return out, err
}
