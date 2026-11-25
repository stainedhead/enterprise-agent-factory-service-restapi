package billingpull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
)

// Google queries the Cloud Billing export in BigQuery (POST /bigquery/v2/projects/<project>/queries) for the
// day's Vertex AI cost, credits included, per afs-team-id label (resource label, else project label). Token is
// a Google OAuth bearer token (live federation is deferred: see implementation notes).
type Google struct {
	// BaseURL defaults to https://bigquery.googleapis.com.
	BaseURL string
	Client  Doer
	Token   Credential
	// Exports maps a route slug to "<job project>:<dataset>.<table>" of the billing export.
	Exports map[string]string
}

// Provider implements routebill.Puller.
func (g *Google) Provider() string { return "google" }

var exportRE = regexp.MustCompile(`^([a-z][a-z0-9-]{4,28}[a-z0-9]):([A-Za-z0-9_]{1,1000})\.([A-Za-z0-9_]{1,1000})$`)

const googleQuery = "SELECT COALESCE((SELECT l.value FROM UNNEST(labels) l WHERE l.key = @tag), " +
	"(SELECT l.value FROM UNNEST(project.labels) l WHERE l.key = @tag), '') AS team, " +
	"SUM(cost) + SUM(IFNULL((SELECT SUM(c.amount) FROM UNNEST(credits) c), 0)) AS cost " +
	"FROM `%s.%s` WHERE service.description = 'Vertex AI' AND currency = 'USD' " +
	"AND usage_start_time >= TIMESTAMP(@from) AND usage_start_time < TIMESTAMP(@to) GROUP BY team"

type bqResponse struct {
	JobComplete bool `json:"jobComplete"`
	Rows        []struct {
		F []struct {
			V *string `json:"v"`
		} `json:"f"`
	} `json:"rows"`
	PageToken string `json:"pageToken"`
}

// Pull implements routebill.Puller. A query that does not finish within the request is an error (polling
// getQueryResults is not needed at a team's daily row count).
func (g *Google) Pull(ctx context.Context, ep routebill.Endpoint, day time.Time) ([]routebill.Figure, domain.Money, error) {
	m := exportRE.FindStringSubmatch(g.Exports[ep.Slug])
	if m == nil {
		return nil, domain.Money{}, fmt.Errorf("billingpull: google: no billing export for route %s", ep.Slug)
	}
	token, err := g.Token(ctx, ep)
	if err != nil {
		return nil, domain.Money{}, err
	}
	base := g.BaseURL
	if base == "" {
		base = "https://bigquery.googleapis.com"
	}
	start := day.UTC().Truncate(24 * time.Hour)
	param := func(name, v string) map[string]any {
		return map[string]any{"name": name, "parameterType": map[string]string{"type": "STRING"}, "parameterValue": map[string]string{"value": v}}
	}
	body, _ := json.Marshal(map[string]any{ // plain maps always marshal
		"query": fmt.Sprintf(googleQuery, m[2], m[3]), "useLegacySql": false, "parameterMode": "NAMED", "timeoutMs": 60000,
		"queryParameters": []map[string]any{param("tag", TeamTag), param("from", start.Format(time.RFC3339)),
			param("to", start.Add(24*time.Hour).Format(time.RFC3339))},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/bigquery/v2/projects/"+m[1]+"/queries", bytes.NewReader(body))
	if err != nil {
		return nil, domain.Money{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	var res bqResponse
	if err := call(g.Client, req, token, &res); err != nil {
		return nil, domain.Money{}, err
	}
	if !res.JobComplete || res.PageToken != "" {
		return nil, domain.Money{}, fmt.Errorf("%w: google: the billing export query did not complete in one response", ErrProvider)
	}
	var a attribution
	for _, r := range res.Rows {
		if len(r.F) != 2 || r.F[1].V == nil {
			continue
		}
		cost, err := strconv.ParseFloat(*r.F[1].V, 64)
		if err != nil {
			return nil, domain.Money{}, fmt.Errorf("%w: google: cost %q", ErrProvider, *r.F[1].V)
		}
		team, ok := "", false
		if r.F[0].V != nil {
			team, ok = TeamOfFolded(*r.F[0].V) // labels are lower case
		}
		a.add(team, ok, usd(cost))
	}
	figs, un := a.result()
	return figs, un, nil
}
