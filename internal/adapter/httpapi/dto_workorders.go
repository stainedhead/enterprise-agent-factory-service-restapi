package httpapi

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type workOrderProgressBody struct {
	Repos           int `json:"repos"`
	PRsOpen         int `json:"prsOpen"`
	PRsMerged       int `json:"prsMerged"`
	NotAffected     int `json:"notAffected"`
	FailedOrSkipped int `json:"failedOrSkipped"`
	Remaining       int `json:"remaining"`
}

type workOrderNotificationBody struct {
	Kind      string `json:"kind"`
	Recipient string `json:"recipient"`
	SentAt    string `json:"sentAt"`
}

type workOrderRepoBody struct {
	Host          string                      `json:"host"`
	Repo          string                      `json:"repo"`
	OwningTeamID  *string                     `json:"owningTeamId"`
	OwningContact *string                     `json:"owningContact"`
	OwnerSource   string                      `json:"ownerSource,omitempty"`
	Status        string                      `json:"status"`
	PRURL         *string                     `json:"prUrl"`
	PRNumber      *int                        `json:"prNumber"`
	Detail        *string                     `json:"detail"`
	MergedAt      *string                     `json:"mergedAt"`
	Notifications []workOrderNotificationBody `json:"notifications"`
	Version       int64                       `json:"version"`
	UpdatedAt     string                      `json:"updatedAt"`
}

type workOrderBody struct {
	ID           string                `json:"id"`
	Version      int64                 `json:"version"`
	CreatedAt    string                `json:"createdAt"`
	CreatedBy    string                `json:"createdBy"`
	UpdatedAt    string                `json:"updatedAt"`
	Kind         string                `json:"kind"`
	CVEID        *string               `json:"cveId"`
	Severity     string                `json:"severity"`
	Title        string                `json:"title"`
	Source       string                `json:"source"`
	TemplateID   string                `json:"templateId"`
	BudgetUSD    string                `json:"budgetUsd"`
	SLADueAt     string                `json:"slaDueAt"`
	Status       string                `json:"status"`
	TeamID       *string               `json:"teamId"`
	AssignedAt   *string               `json:"assignedAt"`
	CompletedAt  *string               `json:"completedAt"`
	SLAState     string                `json:"slaState"`
	Progress     workOrderProgressBody `json:"progress"`
	CancelReason *string               `json:"cancelReason"`
	Repos        *[]workOrderRepoBody  `json:"repos,omitempty"`
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toWorkOrderRepoBody(r domain.WorkOrderRepo) workOrderRepoBody {
	b := workOrderRepoBody{Host: r.Host, Repo: r.Repo, OwningTeamID: strOrNil(r.OwningTeamID), OwningContact: strOrNil(r.OwningContact),
		OwnerSource: string(r.OwnerSource), Status: string(r.Status), PRURL: strOrNil(r.PRURL), Detail: strOrNil(r.Detail),
		MergedAt: w2TimePtr(r.MergedAt), Notifications: make([]workOrderNotificationBody, 0, len(r.Notifications)),
		Version: int64(r.Version), UpdatedAt: w2Time(r.UpdatedAt)}
	if r.PRNumber > 0 {
		n := r.PRNumber
		b.PRNumber = &n
	}
	for _, n := range r.Notifications {
		b.Notifications = append(b.Notifications, workOrderNotificationBody{Kind: n.Kind, Recipient: n.Recipient, SentAt: w2Time(n.SentAt)})
	}
	return b
}

// toWorkOrderBody renders a work order; withRepos adds its repositories (GetWorkOrder, CreateWorkOrder).
func toWorkOrderBody(w domain.WorkOrder, withRepos bool) workOrderBody {
	b := workOrderBody{ID: w.ID, Version: int64(w.Version), CreatedAt: w2Time(w.CreatedAt), CreatedBy: w.CreatedBy, UpdatedAt: w2Time(w.UpdatedAt),
		Kind: string(w.Kind), CVEID: strOrNil(w.CVEID), Severity: string(w.Severity), Title: w.Title, Source: string(w.Source),
		TemplateID: w.TemplateID, BudgetUSD: w.Budget.String(), SLADueAt: w2Time(w.SLADueAt), Status: string(w.Status),
		TeamID: strOrNil(w.TeamID), AssignedAt: w2TimePtr(w.AssignedAt), CompletedAt: w2TimePtr(w.CompletedAt), SLAState: string(w.SLAState),
		Progress: workOrderProgressBody{Repos: w.Progress.Repos, PRsOpen: w.Progress.PRsOpen, PRsMerged: w.Progress.PRsMerged,
			NotAffected: w.Progress.NotAffected, FailedOrSkipped: w.Progress.FailedOrSkipped, Remaining: w.Progress.Remaining},
		CancelReason: strOrNil(w.CancelReason)}
	if withRepos {
		repos := make([]workOrderRepoBody, 0, len(w.Repos))
		for _, r := range w.Repos {
			repos = append(repos, toWorkOrderRepoBody(r))
		}
		b.Repos = &repos
	}
	return b
}

type workOrderRepoInput struct {
	Host          string `json:"host"`
	Repo          string `json:"repo"`
	OwningTeamID  string `json:"owningTeamId"`
	OwningContact string `json:"owningContact"`
}

func toRepoInputs(in []workOrderRepoInput) []domain.RepoInput {
	out := make([]domain.RepoInput, len(in))
	for i, r := range in {
		out[i] = domain.RepoInput{Host: r.Host, Repo: r.Repo, OwningTeamID: r.OwningTeamID, OwningContact: r.OwningContact}
	}
	return out
}

type createWorkOrderRequest struct {
	Kind       string               `json:"kind"`
	CVEID      string               `json:"cveId"`
	Severity   string               `json:"severity"`
	Title      string               `json:"title"`
	SLADueAt   string               `json:"slaDueAt"`
	TemplateID string               `json:"templateId"`
	BudgetUSD  string               `json:"budgetUsd"`
	Repos      []workOrderRepoInput `json:"repos"`
}

type updateWorkOrderRequest struct {
	Title     *string              `json:"title"`
	SLADueAt  *string              `json:"slaDueAt"`
	BudgetUSD *string              `json:"budgetUsd"`
	AddRepos  []workOrderRepoInput `json:"addRepos"`
}

func (u updateWorkOrderRequest) patch() (domain.WorkOrderPatch, error) {
	p := domain.WorkOrderPatch{Title: u.Title, BudgetUSD: u.BudgetUSD}
	if u.SLADueAt != nil {
		t, err := w2ParseTime("slaDueAt", *u.SLADueAt)
		if err != nil {
			return p, err
		}
		p.SLADueAt = &t
	}
	return p, nil
}

type updateWorkOrderRepoRequest struct {
	Status   string  `json:"status"`
	PRURL    string  `json:"prUrl"`
	PRNumber int     `json:"prNumber"`
	Detail   *string `json:"detail"`
}
