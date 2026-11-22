package domain

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Work order limits (database 000001 and 000012; API contract §4.10a).
const (
	// MaxWorkOrderRepos bounds the repositories of one work order (the tracking budget of RC-12 is 500).
	MaxWorkOrderRepos = 500
	// MaxWorkOrderTitle is the title length limit of API contract §5.
	MaxWorkOrderTitle = 200
	// MaxRepoDetail is the database check on work_order_repos.detail.
	MaxRepoDetail = 1000
	// MaxCancelReason bounds the reason of CancelWorkOrder.
	MaxCancelReason = 500
	// DefaultRepoHost is the host of a repository that names none.
	DefaultRepoHost = "github.com"
	// DefaultSLAWarningHours is the default of the work_orders.sla_warning_hours service setting.
	DefaultSLAWarningHours = 48
)

var (
	cveRE       = regexp.MustCompile(`^CVE-[0-9]{4}-[0-9]{4,}$`)
	repoHostRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	repoPathRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	contactRE   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	teamIDRE    = regexp.MustCompile(`^team_[0-9A-HJKMNP-TV-Z]{26}$`)
	prURLRE     = regexp.MustCompile(`^https://\S+$`)
	templateIDR = regexp.MustCompile(`^swt_[0-9A-HJKMNP-TV-Z]{26}$`)
)

// WorkOrderProgress counts a work order's repositories by state (afs.work_order_progress).
type WorkOrderProgress struct {
	Repos           int
	PRsOpen         int
	PRsMerged       int
	NotAffected     int
	FailedOrSkipped int
	Remaining       int
}

// WorkOrderNotification is one notification sent about a repository, once per kind and recipient.
type WorkOrderNotification struct {
	Kind      string
	Recipient string
	SentAt    time.Time
}

// WorkOrderRepo is one repository of a work order and the state of its fix. OwnerSource is known only
// in the CreateWorkOrder response (it is not stored).
type WorkOrderRepo struct {
	WorkOrderID   string
	Host          string
	Repo          string
	OwningTeamID  string
	OwningContact string
	OwnerSource   OwnerSource
	Status        RepoStatus
	PRURL         string
	PRNumber      int
	Detail        string
	MergedAt      *time.Time
	Notifications []WorkOrderNotification
	Version       Version
	UpdatedAt     time.Time
}

// Key is the repository's identity within its work order, case-insensitive.
func (r WorkOrderRepo) Key() string { return strings.ToLower(r.Host + "/" + r.Repo) }

// WorkOrder is a unit of work an operator or a security tool asks AFS to do, normally a CVE
// remediation across repositories by a swarm instantiated from a template (RA-9, RC-12).
type WorkOrder struct {
	ID           string
	Kind         WorkOrderKind
	CVEID        string
	Severity     Severity
	Title        string
	Source       WorkOrderSource
	TemplateID   string
	Budget       Money
	SLADueAt     time.Time
	Status       WorkOrderStatus
	TeamID       string
	AssignedAt   *time.Time
	CompletedAt  *time.Time
	CancelReason string
	Progress     WorkOrderProgress
	Repos        []WorkOrderRepo
	// SLAState is derived at response time (SLAStateAt); it is not stored.
	SLAState  SLAState
	Version   Version
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
}

// Open reports whether the work order is not done, failed or cancelled.
func (w WorkOrder) Open() bool { return OpenWorkOrderStatus(w.Status) }

// OpenWorkOrderStatus reports whether a work order in status s is still open.
func OpenWorkOrderStatus(s WorkOrderStatus) bool {
	return s != WorkOrderStatusDone && s != WorkOrderStatusFailed && s != WorkOrderStatusCancelled
}

// SLAStateAt is the SLA state at now, the rule of the afs.work_order_progress view: a work order that
// is done or cancelled has met or missed its SLA by its completion time; an open one is breached past
// the due time and due soon within warnHours before it.
func (w WorkOrder) SLAStateAt(now time.Time, warnHours int) SLAState {
	if w.SLADueAt.IsZero() {
		return ""
	}
	if w.Status == WorkOrderStatusDone || w.Status == WorkOrderStatusCancelled {
		done := now
		if w.CompletedAt != nil {
			done = *w.CompletedAt
		}
		if !done.After(w.SLADueAt) {
			return SLAStateMet
		}
		return SLAStateMissed
	}
	switch {
	case now.After(w.SLADueAt):
		return SLAStateBreached
	case now.After(w.SLADueAt.Add(-time.Duration(warnHours) * time.Hour)):
		return SLAStateDueSoon
	}
	return SLAStateOnTrack
}

// RepoInput is a repository of a CreateWorkOrder or UpdateWorkOrder body: it may name its owner
// (a team, a contact or both) or none.
type RepoInput struct {
	Host          string
	Repo          string
	OwningTeamID  string
	OwningContact string
}

// HasOwner reports whether the input names an owner.
func (r RepoInput) HasOwner() bool { return r.OwningTeamID != "" || r.OwningContact != "" }

// NormalizeRepoInputs validates the repositories of a request: host (default github.com, lower case),
// owner/name path, contact address and team id formats, and no repository twice. field names the
// request member ("repos" or "addRepos"). A non-empty list is required when required is set.
func NormalizeRepoInputs(field string, in []RepoInput, required bool) ([]RepoInput, error) {
	var fe []FieldError
	if len(in) == 0 && required {
		fe = append(fe, FieldError{Field: field, Reason: "needs at least one repository"})
	}
	if len(in) > MaxWorkOrderRepos {
		fe = append(fe, FieldError{Field: field, Reason: "has at most " + itoa(MaxWorkOrderRepos) + " repositories"})
	}
	out := make([]RepoInput, 0, len(in))
	seen := map[string]bool{}
	for i, r := range in {
		at := field + "[" + itoa(i) + "]"
		r.Host = strings.ToLower(strings.TrimSpace(r.Host))
		if r.Host == "" {
			r.Host = DefaultRepoHost
		}
		if !repoHostRE.MatchString(r.Host) || len(r.Host) > 253 {
			fe = append(fe, FieldError{Field: at + ".host", Reason: "must be a host name such as github.com"})
		}
		if !repoPathRE.MatchString(r.Repo) {
			fe = append(fe, FieldError{Field: at + ".repo", Reason: "must be owner/name"})
		}
		if r.OwningTeamID != "" && !teamIDRE.MatchString(r.OwningTeamID) {
			fe = append(fe, FieldError{Field: at + ".owningTeamId", Reason: "must be a team id (team_…)"})
		}
		if r.OwningContact != "" && (!contactRE.MatchString(r.OwningContact) || len(r.OwningContact) > 254) {
			fe = append(fe, FieldError{Field: at + ".owningContact", Reason: "must be an email address"})
		}
		key := strings.ToLower(r.Host + "/" + r.Repo)
		if seen[key] {
			fe = append(fe, FieldError{Field: at, Reason: "repeats " + key})
		}
		seen[key] = true
		out = append(out, r)
	}
	if len(fe) > 0 {
		return nil, Validation(fe...)
	}
	return out, nil
}

// NewWorkOrderParams is a CreateWorkOrder body without its repositories.
type NewWorkOrderParams struct {
	Kind       string
	CVEID      string
	Severity   string
	Title      string
	SLADueAt   time.Time
	TemplateID string
	BudgetUSD  string
}

func validateWorkOrderTitle(t string) []FieldError {
	n := utf8.RuneCountInString(strings.TrimSpace(t))
	if n < 1 || n > MaxWorkOrderTitle {
		return []FieldError{{Field: "title", Reason: "must be 1 to " + itoa(MaxWorkOrderTitle) + " characters"}}
	}
	return nil
}

// NewWorkOrder validates p and returns the open work order at version 1; budget is the template's
// budget, used when the body names none. The id is set by the caller.
func NewWorkOrder(id string, p NewWorkOrderParams, templateBudget Money, now time.Time) (WorkOrder, error) {
	var fe []FieldError
	kind := WorkOrderKind(p.Kind)
	if p.Kind == "" {
		kind = WorkOrderKindGeneral
		if p.CVEID != "" {
			kind = WorkOrderKindCveRemediation
		}
	}
	if !kind.Valid() {
		fe = append(fe, FieldError{Field: "kind", Reason: "must be cve_remediation or general"})
	}
	if p.CVEID != "" && !cveRE.MatchString(p.CVEID) {
		fe = append(fe, FieldError{Field: "cveId", Reason: "must look like CVE-2026-12345"})
	}
	if kind == WorkOrderKindCveRemediation && p.CVEID == "" {
		fe = append(fe, FieldError{Field: "cveId", Reason: "is required for a cve_remediation work order"})
	}
	sev := Severity(p.Severity)
	if !sev.Valid() {
		fe = append(fe, FieldError{Field: "severity", Reason: "must be critical, high, medium or low"})
	}
	fe = append(fe, validateWorkOrderTitle(p.Title)...)
	switch {
	case p.SLADueAt.IsZero():
		fe = append(fe, FieldError{Field: "slaDueAt", Reason: "is required"})
	case !p.SLADueAt.After(now):
		fe = append(fe, FieldError{Field: "slaDueAt", Reason: "must be in the future"})
	}
	if !templateIDR.MatchString(p.TemplateID) {
		fe = append(fe, FieldError{Field: "templateId", Reason: "must be a swarm template id (swt_…)"})
	}
	budget := templateBudget
	if p.BudgetUSD != "" {
		m, bfe := validateTemplateBudget("budgetUsd", p.BudgetUSD)
		fe = append(fe, bfe...)
		budget = m
	}
	if len(fe) > 0 {
		return WorkOrder{}, Validation(fe...)
	}
	return WorkOrder{ID: id, Kind: kind, CVEID: p.CVEID, Severity: sev, Title: strings.TrimSpace(p.Title), Source: WorkOrderSourceApi,
		TemplateID: p.TemplateID, Budget: budget, SLADueAt: p.SLADueAt.UTC(), Status: WorkOrderStatusOpen,
		Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

// WorkOrderPatch is an UpdateWorkOrder body without addRepos: a nil field is left unchanged.
type WorkOrderPatch struct {
	Title     *string
	SLADueAt  *time.Time
	BudgetUSD *string
}

// Empty reports whether the patch changes no work order field.
func (p WorkOrderPatch) Empty() bool {
	return p.Title == nil && p.SLADueAt == nil && p.BudgetUSD == nil
}

func closedConflict(w WorkOrder) error {
	return Conflict("work_order_closed", "work order is "+string(w.Status)+" and cannot change", map[string]any{"status": string(w.Status)})
}

// Apply returns the work order with the patch applied. A closed work order cannot change. The
// remediation team already created keeps its budget: only a team created later reads the new cap.
func (w WorkOrder) Apply(p WorkOrderPatch, now time.Time) (WorkOrder, error) {
	if !w.Open() {
		return w, closedConflict(w)
	}
	var fe []FieldError
	if p.Title != nil {
		fe = append(fe, validateWorkOrderTitle(*p.Title)...)
		w.Title = strings.TrimSpace(*p.Title)
	}
	if p.SLADueAt != nil {
		if !p.SLADueAt.After(now) {
			fe = append(fe, FieldError{Field: "slaDueAt", Reason: "must be in the future"})
		}
		w.SLADueAt = p.SLADueAt.UTC()
	}
	if p.BudgetUSD != nil {
		m, bfe := validateTemplateBudget("budgetUsd", *p.BudgetUSD)
		fe = append(fe, bfe...)
		w.Budget = m
	}
	if len(fe) > 0 {
		return w, Validation(fe...)
	}
	w.UpdatedAt = now.UTC()
	return w, nil
}

// CanAddRepos reports a Conflict when the work order is closed.
func (w WorkOrder) CanAddRepos() error {
	if !w.Open() {
		return closedConflict(w)
	}
	return nil
}

// Cancel returns the work order cancelled with the reason; the reconciler tears the team down. A work
// order that is done, failed or already cancelled cannot be cancelled.
func (w WorkOrder) Cancel(reason string, now time.Time) (WorkOrder, error) {
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 1 || n > MaxCancelReason {
		return w, Validation(FieldError{Field: "reason", Reason: "must be 1 to " + itoa(MaxCancelReason) + " characters"})
	}
	if !w.Open() {
		return w, closedConflict(w)
	}
	t := now.UTC()
	w.Status, w.CancelReason, w.CompletedAt, w.UpdatedAt = WorkOrderStatusCancelled, reason, &t, t
	return w, nil
}

// Audit is the audit view of the work order.
func (w WorkOrder) Audit() map[string]any {
	return map[string]any{"id": w.ID, "kind": string(w.Kind), "cveId": w.CVEID, "severity": string(w.Severity), "title": w.Title,
		"templateId": w.TemplateID, "budgetUsd": w.Budget.String(), "slaDueAt": w.SLADueAt, "status": string(w.Status),
		"teamId": w.TeamID, "cancelReason": w.CancelReason}
}

// Terminal reports whether a repository in status s is finished for good: a merged pull request does
// not go back.
func (s RepoStatus) Terminal() bool { return s == RepoStatusMerged }

// RepoReport is an UpdateWorkOrderRepo body: the new status and optional pull request facts.
type RepoReport struct {
	Status   string
	PRURL    string
	PRNumber int
	Detail   *string
}

// Apply returns the repository with the report applied. pr_opened and merged need a pull request URL
// (the report's, or the one already recorded); a merged repository does not change again.
func (r WorkOrderRepo) Apply(rep RepoReport, now time.Time) (WorkOrderRepo, error) {
	var fe []FieldError
	st := RepoStatus(rep.Status)
	if !st.Valid() {
		fe = append(fe, FieldError{Field: "status", Reason: "must be pending, in_progress, pr_opened, merged, not_affected, failed or skipped"})
	}
	if rep.PRURL != "" {
		if !prURLRE.MatchString(rep.PRURL) || len(rep.PRURL) > 500 {
			fe = append(fe, FieldError{Field: "prUrl", Reason: "must be an https URL of at most 500 characters"})
		}
		r.PRURL = rep.PRURL
	}
	if rep.PRNumber != 0 {
		if rep.PRNumber < 1 {
			fe = append(fe, FieldError{Field: "prNumber", Reason: "must be at least 1"})
		}
		r.PRNumber = rep.PRNumber
	}
	if rep.Detail != nil {
		if utf8.RuneCountInString(*rep.Detail) > MaxRepoDetail {
			fe = append(fe, FieldError{Field: "detail", Reason: "must be at most " + itoa(MaxRepoDetail) + " characters"})
		}
		r.Detail = *rep.Detail
	}
	if (st == RepoStatusPrOpened || st == RepoStatusMerged) && r.PRURL == "" {
		fe = append(fe, FieldError{Field: "prUrl", Reason: "is required when status is " + string(st)})
	}
	if len(fe) > 0 {
		return r, Validation(fe...)
	}
	if r.Status.Terminal() && st != r.Status {
		return r, Conflict("repo_merged", "a merged repository does not change status", map[string]any{"status": string(r.Status)})
	}
	if st == RepoStatusMerged && r.Status != RepoStatusMerged {
		t := now.UTC()
		r.MergedAt = &t
	}
	r.Status, r.UpdatedAt = st, now.UTC()
	return r, nil
}

// Audit is the audit view of the repository.
func (r WorkOrderRepo) Audit() map[string]any {
	return map[string]any{"host": r.Host, "repo": r.Repo, "owningTeamId": r.OwningTeamID, "owningContact": r.OwningContact,
		"status": string(r.Status), "prUrl": r.PRURL, "prNumber": r.PRNumber}
}

// AmbiguousRepo is a repository two or more teams own at the best match.
type AmbiguousRepo struct {
	Repo    string
	TeamIDs []string
}

// OwnerResolutionError is the ValidationError of a CreateWorkOrder or UpdateWorkOrder whose
// repositories name no owner and do not resolve to one team (D38). Nothing is written.
func OwnerResolutionError(ambiguous []AmbiguousRepo, unowned []string) error {
	e := Validation()
	var msg []string
	e.Details = map[string]any{}
	if len(ambiguous) > 0 {
		list := make([]map[string]any, 0, len(ambiguous))
		for _, a := range ambiguous {
			list = append(list, map[string]any{"repo": a.Repo, "teamIds": a.TeamIDs})
		}
		e.Details["ambiguousRepos"] = list
		msg = append(msg, "more than one team owns "+strings.Join(repoNames(ambiguous), ", ")+"; give the owner as owningTeamId or owningContact")
	}
	if len(unowned) > 0 {
		e.Details["unownedRepos"] = unowned
		msg = append(msg, "no team lists "+strings.Join(unowned, ", ")+"; give =team_… or =email as its owner")
	}
	e.Message = strings.Join(msg, "; ")
	return e
}

func repoNames(a []AmbiguousRepo) []string {
	out := make([]string, len(a))
	for i, x := range a {
		out[i] = x.Repo
	}
	return out
}

// ValidTeamID reports whether s is shaped like a team id (team_ and a 26 character ULID).
func ValidTeamID(s string) bool { return teamIDRE.MatchString(s) }
