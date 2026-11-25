// Package billingpull holds afs-billing's provider cost pulls (routebill.Puller) and the LiteLLM price list
// (MEG-BL-1, MEG-BL-3). Each puller reads one provider's own figures for a route over HTTPS with a credential it
// is handed per call (an admin key read from <prefix>/_service/billing/<route>, or a bearer token for Azure and
// Google); no credential is stored, logged or put in an error.
//
// Attribution (the per_team credential convention): a figure belongs to a team when the provider side names
// the team: an OpenAI project or OpenRouter key whose name contains the team id, an Azure resource group whose
// name contains it, or an afs-team-id tag (Azure) or label (Google) holding it. Anything else is unattributed.
package billingpull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
)

// TeamTag is the tag (Azure) or label (Google) key that names a team.
const TeamTag = "afs-team-id"

// maxBody bounds a provider response.
const maxBody = 32 << 20

// Doer sends an HTTP request (an *http.Client).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Credential returns the credential a pull for the route uses: an admin key or a bearer token.
type Credential func(ctx context.Context, ep routebill.Endpoint) (string, error)

var teamRE = regexp.MustCompile(`team_[0-9A-HJKMNP-TV-Z]{26}`)

// TeamOf returns the team id a provider-side name contains.
func TeamOf(name string) (string, bool) {
	id := teamRE.FindString(name)
	return id, id != ""
}

// TeamOfFolded is TeamOf on a case-folded name (Google labels and Azure resource group names lose or ignore
// case): the id's ULID part is compared upper-cased.
func TeamOfFolded(name string) (string, bool) {
	return TeamOf(strings.ReplaceAll(strings.ToUpper(name), "TEAM_", "team_"))
}

// usd converts a provider's float amount to Money, rounded to the micro-dollar.
func usd(v float64) domain.Money { return domain.MoneyFromMicros(int64(math.Round(v * 1e6))) }

// ErrProvider is wrapped by every provider failure.
var ErrProvider = errors.New("billingpull: provider request failed")

// call sends req with the bearer credential and decodes a JSON response into out. The error names the host
// and status, never the credential or the body.
func call(c Doer, req *http.Request, bearer string, out any) error {
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %s", ErrProvider, req.URL.Host, redactErr(err, bearer))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: %s: read: %s", ErrProvider, req.URL.Host, redactErr(err, bearer))
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: %s %s: status %d", ErrProvider, req.Method, req.URL.Host+req.URL.Path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s: decode: %w", ErrProvider, req.URL.Host+req.URL.Path, err)
	}
	return nil
}

func redactErr(err error, secret string) string {
	s := err.Error()
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}

// attribution sums figures per team; names without a team go to unattributed.
type attribution struct {
	by map[string]domain.Money
	un domain.Money
}

func (a *attribution) add(team string, ok bool, amount domain.Money) {
	if !ok {
		a.un = a.un.Add(amount)
		return
	}
	if a.by == nil {
		a.by = map[string]domain.Money{}
	}
	a.by[team] = a.by[team].Add(amount)
}

func (a *attribution) result() ([]routebill.Figure, domain.Money) {
	out := make([]routebill.Figure, 0, len(a.by))
	for t, m := range a.by {
		out = append(out, routebill.Figure{TeamID: t, AmountUSD: m})
	}
	sortFigures(out)
	return out, a.un
}

func sortFigures(f []routebill.Figure) {
	slices.SortFunc(f, func(a, b routebill.Figure) int { return strings.Compare(a.TeamID, b.TeamID) })
}
