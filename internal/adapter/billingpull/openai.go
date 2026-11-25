package billingpull

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
)

// OpenAI pulls the organization costs API grouped by project (GET /v1/organization/costs, bucket_width 1d,
// group_by project_id) with the route's admin key, and names each project's team from the project name
// (GET /v1/organization/projects).
type OpenAI struct {
	// BaseURL defaults to https://api.openai.com.
	BaseURL string
	Client  Doer
	Key     Credential
}

// Provider implements routebill.Puller.
func (o *OpenAI) Provider() string { return "openai" }

func (o *OpenAI) base() string {
	if o.BaseURL == "" {
		return "https://api.openai.com"
	}
	return strings.TrimRight(o.BaseURL, "/")
}

type openAICosts struct {
	Data []struct {
		Results []struct {
			Amount struct {
				Value    float64 `json:"value"`
				Currency string  `json:"currency"`
			} `json:"amount"`
			ProjectID *string `json:"project_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

type openAIProjects struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

// Pull implements routebill.Puller.
func (o *OpenAI) Pull(ctx context.Context, ep routebill.Endpoint, day time.Time) ([]routebill.Figure, domain.Money, error) {
	key, err := o.Key(ctx, ep)
	if err != nil {
		return nil, domain.Money{}, err
	}
	names, err := o.projects(ctx, key)
	if err != nil {
		return nil, domain.Money{}, err
	}
	start := day.UTC().Truncate(24 * time.Hour)
	q := url.Values{"start_time": {strconv.FormatInt(start.Unix(), 10)}, "end_time": {strconv.FormatInt(start.Add(24*time.Hour).Unix(), 10)},
		"bucket_width": {"1d"}, "group_by": {"project_id"}, "limit": {"31"}}
	var a attribution
	for page := ""; ; {
		if page != "" {
			q.Set("page", page)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base()+"/v1/organization/costs?"+q.Encode(), nil)
		if err != nil {
			return nil, domain.Money{}, err
		}
		var body openAICosts
		if err := call(o.Client, req, key, &body); err != nil {
			return nil, domain.Money{}, err
		}
		for _, b := range body.Data {
			for _, r := range b.Results {
				name := ""
				if r.ProjectID != nil {
					name = names[*r.ProjectID]
				}
				team, ok := TeamOf(name)
				a.add(team, ok, usd(r.Amount.Value))
			}
		}
		if !body.HasMore || body.NextPage == nil || *body.NextPage == "" {
			break
		}
		page = *body.NextPage
	}
	figs, un := a.result()
	return figs, un, nil
}

func (o *OpenAI) projects(ctx context.Context, key string) (map[string]string, error) {
	out := map[string]string{}
	q := url.Values{"limit": {"100"}, "include_archived": {"true"}}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base()+"/v1/organization/projects?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var body openAIProjects
		if err := call(o.Client, req, key, &body); err != nil {
			return nil, err
		}
		for _, p := range body.Data {
			out[p.ID] = p.Name
		}
		if !body.HasMore || body.LastID == "" {
			return out, nil
		}
		q.Set("after", body.LastID)
	}
}
