// Package routebill is afs-billing's work on model routes (PRD §9.4, MEG-BL-1..3):
//
//   - daily provider pulls (OpenAI organization costs, OpenRouter key usage, Azure Cost Management, Google
//     billing export) compared per team with the gateway's settled cost of the route;
//   - monthly provider statements (Nous Portal, OpenCode usage CSV) compared the same way, and a route with a
//     statement source but no statement for the month reported as not reconciled;
//   - the daily allocation of capacity routes' cost to agents and pools by token share (cost_daily source
//     route_capacity);
//   - route price proposals from a price list.
//
// A variance above billing.variance_alert_pct publishes cost.variance_detected. Provider figures are attributed
// to a team by the provider side naming the team (a project, key, resource group or label per team, the
// per_team credential mode); unattributed figures are reported in the result, never guessed.
package routebill

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Endpoint is a route the billing job works on.
type Endpoint struct {
	ID, Slug, Provider string
}

// Figure is a provider's own cost of one team on a route for a day or a month.
type Figure struct {
	TeamID    string
	AmountUSD domain.Money
}

// Puller reads one provider's daily cost figures for a route.
type Puller interface {
	// Provider is the inference provider key it serves (openai, openrouter, azure, google).
	Provider() string
	// Pull returns the provider's cost of the route on day, per team, and the amount it could not attribute.
	Pull(ctx context.Context, ep Endpoint, day time.Time) (figures []Figure, unattributed domain.Money, err error)
}

// Statement is an imported monthly statement, summed per team.
type Statement struct {
	Endpoint Endpoint
	Source   string
	Figures  []Figure
}

// Capacity is the cost of a capacity route for one team in force on a day.
type Capacity struct {
	Endpoint Endpoint
	TeamID   string
	// MonthlyUSD or HourlyUSD; Allocation is by_tokens or by_agent.
	MonthlyUSD, HourlyUSD *domain.Money
	Allocation            string
}

// Share is the tokens one subject used on a route in a day.
type Share struct {
	AgentID, PoolID string
	Tokens          int64
}

// CostRow is one cost_daily row of source route_capacity.
type CostRow struct {
	TeamID, AgentID, PoolID string
	// ModelID is the route slug (route_capacity may name the route alone).
	ModelID   string
	AmountUSD domain.Money
	Tokens    int64
}

// ListedPrice is a route model's price from a price list, USD per million tokens.
type ListedPrice struct {
	Provider, Model string
	Input, Output   domain.Money
}

// RouteModel is a per_token route model with its price in force and whether a proposal is pending.
type RouteModel struct {
	Endpoint Endpoint
	Model    string
	// Current is nil when the model has no price in force.
	Current *[2]domain.Money
	// PendingID is the open proposal, "" when none; Pending its prices.
	PendingID string
	Pending   [2]domain.Money
}

// Proposal is a new pending route price proposal.
type Proposal struct {
	ID, EndpointID, Model string
	Input, Output         domain.Money
	EffectiveFrom         time.Time
	Note                  string
}

// Store is the billing store, used inside a transaction.
type Store interface {
	// Threshold is billing.variance_alert_pct.
	Threshold(ctx context.Context, tx port.Tx) (float64, error)
	// Endpoints lists the enabled routes of the providers.
	Endpoints(ctx context.Context, tx port.Tx, providers []string) ([]Endpoint, error)
	// GatewayCost sums the settled cost of the route's calls per team in [from, to).
	GatewayCost(ctx context.Context, tx port.Tx, endpointID string, from, to time.Time) (map[string]domain.Money, error)
	// Statements returns the statements of the month; StatementRoutes the routes whose provider is reconciled
	// from statements (nous, opencode).
	Statements(ctx context.Context, tx port.Tx, period time.Time) ([]Statement, error)
	StatementRoutes(ctx context.Context, tx port.Tx) ([]Endpoint, error)
	// Capacities lists the capacity costs in force on day; Shares the tokens per subject of a team on a route.
	Capacities(ctx context.Context, tx port.Tx, day time.Time) ([]Capacity, error)
	Shares(ctx context.Context, tx port.Tx, endpointID, teamID string, day time.Time) ([]Share, error)
	// ReplaceCapacity replaces the day's route_capacity rows of cost_daily.
	ReplaceCapacity(ctx context.Context, tx port.Tx, day time.Time, rows []CostRow) error
	// RouteModels lists the per_token route models; Supersede and Insert manage proposals.
	RouteModels(ctx context.Context, tx port.Tx) ([]RouteModel, error)
	Supersede(ctx context.Context, tx port.Tx, proposalID string) error
	Insert(ctx context.Context, tx port.Tx, p Proposal) error
}

// Job runs the route billing work.
type Job struct {
	Tx      port.TxRunner
	Outbox  port.OutboxWriter
	Clock   port.Clock
	IDs     port.IDGen
	Store   Store
	Pullers []Puller
	Log     *slog.Logger
	// Session is the database session of the job (system:afs-billing).
	Session port.Session
}

// Variance is one team's comparison on a route.
type Variance struct {
	EndpointID, Route, TeamID string
	GatewayUSD, ProviderUSD   domain.Money
	// Pct is (gateway - provider) / provider * 100; NaN when the provider figure is zero.
	Pct float64
	// Over is true when abs(Pct) is above the threshold (or the provider billed nothing for metered cost).
	Over bool
}

// Result is what a run did.
type Result struct {
	Variances []Variance
	// Unattributed is the provider cost no team could be named for, per route.
	Unattributed map[string]domain.Money
	// NotReconciled lists the statement routes without a statement for the month.
	NotReconciled []string
	Events        int
}

func variance(gw, prov domain.Money, threshold float64) (float64, bool) {
	if prov.Micros() == 0 {
		return math.NaN(), gw.Micros() > 0
	}
	pct := float64(gw.Micros()-prov.Micros()) / float64(prov.Micros()) * 100
	return math.Round(pct*1000) / 1000, math.Abs(pct) > threshold
}

func (j *Job) compare(ep Endpoint, gw map[string]domain.Money, figs []Figure, threshold float64) []Variance {
	prov := map[string]domain.Money{}
	for _, f := range figs {
		prov[f.TeamID] = prov[f.TeamID].Add(f.AmountUSD)
	}
	teams := make([]string, 0, len(gw)+len(prov))
	for t := range gw {
		teams = append(teams, t)
	}
	for t := range prov {
		teams = append(teams, t)
	}
	slices.Sort(teams)
	teams = slices.Compact(teams)
	out := make([]Variance, 0, len(teams))
	for _, t := range teams {
		pct, over := variance(gw[t], prov[t], threshold)
		out = append(out, Variance{EndpointID: ep.ID, Route: ep.Slug, TeamID: t, GatewayUSD: gw[t], ProviderUSD: prov[t], Pct: pct, Over: over})
	}
	return out
}

func (j *Job) publish(ctx context.Context, tx port.Tx, v Variance, period, source string) error {
	id, err := j.IDs.NewID("evt")
	if err != nil {
		return err
	}
	pct := ""
	if !math.IsNaN(v.Pct) {
		pct = fmt.Sprintf("%.3f", v.Pct)
	}
	data := map[string]any{"route": v.Route, "endpointId": v.EndpointID, "period": period, "source": source,
		"gatewayUsd": v.GatewayUSD.String(), "billedUsd": v.ProviderUSD.String(), "variancePct": pct, "status": "over_threshold"}
	ev, err := domain.NewEvent(domain.EventCostVarianceDetected, id.String(), j.Clock.Now(), j.Session.Actor, v.TeamID,
		"route_reconciliation", v.EndpointID+"/"+v.TeamID+"/"+period, data)
	if err != nil {
		return err
	}
	return j.Outbox.Publish(ctx, tx, ev)
}

// pull runs every puller against its routes outside a transaction (network calls), keyed by endpoint id.
func (j *Job) pull(ctx context.Context, eps []Endpoint, day time.Time) (map[string][]Figure, map[string]domain.Money, error) {
	figs, un := map[string][]Figure{}, map[string]domain.Money{}
	var first error
	for _, ep := range eps {
		for _, p := range j.Pullers {
			if p.Provider() != ep.Provider {
				continue
			}
			f, u, err := p.Pull(ctx, ep, day)
			if err != nil {
				if j.Log != nil {
					j.Log.Warn("provider pull failed", "route", ep.Slug, "provider", ep.Provider, "err", err)
				}
				if first == nil {
					first = fmt.Errorf("pull %s: %w", ep.Slug, err)
				}
				continue
			}
			figs[ep.ID] = append(figs[ep.ID], f...)
			if u.Micros() != 0 {
				un[ep.Slug] = un[ep.Slug].Add(u)
			}
		}
	}
	return figs, un, first
}

// PullDay reconciles one UTC day of the pulled providers (MEG-BL-1). A failed pull leaves that route out and is
// returned after the others are reconciled.
func (j *Job) PullDay(ctx context.Context, day time.Time) (Result, error) {
	day = day.UTC().Truncate(24 * time.Hour)
	providers := make([]string, 0, len(j.Pullers))
	for _, p := range j.Pullers {
		providers = append(providers, p.Provider())
	}
	var eps []Endpoint
	if err := j.Tx.ReadTx(ctx, j.Session, func(ctx context.Context, tx port.Tx) error {
		var err error
		eps, err = j.Store.Endpoints(ctx, tx, providers)
		return err
	}); err != nil {
		return Result{}, err
	}
	figs, un, pullErr := j.pull(ctx, eps, day)
	res := Result{Unattributed: un}
	err := j.Tx.InTx(ctx, j.Session, func(ctx context.Context, tx port.Tx) error {
		threshold, err := j.Store.Threshold(ctx, tx)
		if err != nil {
			return err
		}
		for _, ep := range eps {
			f, ok := figs[ep.ID]
			if !ok {
				continue
			}
			gw, err := j.Store.GatewayCost(ctx, tx, ep.ID, day, day.Add(24*time.Hour))
			if err != nil {
				return err
			}
			for _, v := range j.compare(ep, gw, f, threshold) {
				res.Variances = append(res.Variances, v)
				if v.Over {
					if err := j.publish(ctx, tx, v, day.Format(time.DateOnly), ep.Provider+"_pull"); err != nil {
						return err
					}
					res.Events++
				}
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, pullErr
}

// ReconcileMonth compares the month's statements with the gateway cost and lists the statement routes that
// have none (MEG-BL-2, not reconciled).
func (j *Job) ReconcileMonth(ctx context.Context, month time.Time) (Result, error) {
	from := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	var res Result
	err := j.Tx.InTx(ctx, j.Session, func(ctx context.Context, tx port.Tx) error {
		threshold, err := j.Store.Threshold(ctx, tx)
		if err != nil {
			return err
		}
		sts, err := j.Store.Statements(ctx, tx, from)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, st := range sts {
			have[st.Endpoint.ID] = true
			gw, err := j.Store.GatewayCost(ctx, tx, st.Endpoint.ID, from, to)
			if err != nil {
				return err
			}
			for _, v := range j.compare(st.Endpoint, gw, st.Figures, threshold) {
				res.Variances = append(res.Variances, v)
				if v.Over {
					if err := j.publish(ctx, tx, v, from.Format("2006-01"), st.Source); err != nil {
						return err
					}
					res.Events++
				}
			}
		}
		routes, err := j.Store.StatementRoutes(ctx, tx)
		if err != nil {
			return err
		}
		for _, ep := range routes {
			if !have[ep.ID] {
				res.NotReconciled = append(res.NotReconciled, ep.Slug)
			}
		}
		slices.Sort(res.NotReconciled)
		return nil
	})
	return res, err
}

// dayCost is the capacity cost of one day: hourly × 24, or monthly / days in the month.
func dayCost(c Capacity, day time.Time) domain.Money {
	if c.HourlyUSD != nil {
		return domain.MoneyFromMicros(c.HourlyUSD.Micros() * 24)
	}
	if c.MonthlyUSD == nil {
		return domain.Money{}
	}
	days := time.Date(day.Year(), day.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return domain.MoneyFromMicros(c.MonthlyUSD.Micros() / int64(days))
}

// Allocate splits amount by weights; the rounding remainder goes to the largest share, so the parts sum to
// amount exactly.
func Allocate(amount domain.Money, weights []int64) []domain.Money {
	out := make([]domain.Money, len(weights))
	var total int64
	big := 0
	for i, w := range weights {
		total += w
		if w > weights[big] {
			big = i
		}
	}
	if total == 0 || len(weights) == 0 {
		return out
	}
	var sum int64
	for i, w := range weights {
		part := int64(math.Floor(float64(amount.Micros()) * float64(w) / float64(total)))
		out[i] = domain.MoneyFromMicros(part)
		sum += part
	}
	out[big] = out[big].Add(domain.MoneyFromMicros(amount.Micros() - sum))
	return out
}

// AllocateCapacity writes the day's capacity cost of each team's capacity routes to its agents and pools by
// token share (by_agent: equal shares among the subjects that called the route), as cost_daily route_capacity
// rows (MEG-BL-3). A team that made no call on the route that day gets one row for the route alone.
func (j *Job) AllocateCapacity(ctx context.Context, day time.Time) ([]CostRow, error) {
	day = day.UTC().Truncate(24 * time.Hour)
	var rows []CostRow
	err := j.Tx.InTx(ctx, j.Session, func(ctx context.Context, tx port.Tx) error {
		rows = nil
		caps, err := j.Store.Capacities(ctx, tx, day)
		if err != nil {
			return err
		}
		for _, c := range caps {
			amount := dayCost(c, day)
			shares, err := j.Store.Shares(ctx, tx, c.Endpoint.ID, c.TeamID, day)
			if err != nil {
				return err
			}
			weights := make([]int64, len(shares))
			for i, s := range shares {
				weights[i] = s.Tokens
				if c.Allocation == "by_agent" {
					weights[i] = 1
				}
			}
			parts := Allocate(amount, weights)
			if len(shares) == 0 || slices.IndexFunc(parts, func(m domain.Money) bool { return m.Micros() != 0 }) < 0 {
				rows = append(rows, CostRow{TeamID: c.TeamID, ModelID: c.Endpoint.Slug, AmountUSD: amount})
				continue
			}
			for i, s := range shares {
				rows = append(rows, CostRow{TeamID: c.TeamID, AgentID: s.AgentID, PoolID: s.PoolID, ModelID: c.Endpoint.Slug, AmountUSD: parts[i], Tokens: s.Tokens})
			}
		}
		return j.Store.ReplaceCapacity(ctx, tx, day, rows)
	})
	return rows, err
}

// ProposePrices compares a price list with the per_token route models and proposes one pending price per model
// whose listed price differs from the price in force (MEG-BL-3). A list entry matches a route model on the
// route's provider and the model name (case-insensitive, an entry "<provider>/<model>" also matches).
func (j *Job) ProposePrices(ctx context.Context, list []ListedPrice) (proposed, superseded int, err error) {
	find := func(provider, model string) (ListedPrice, bool) {
		for _, p := range list {
			name := strings.ToLower(p.Model)
			if (p.Provider == "" || p.Provider == provider) &&
				(name == strings.ToLower(model) || name == strings.ToLower(provider+"/"+model)) {
				return p, true
			}
		}
		return ListedPrice{}, false
	}
	now := j.Clock.Now().UTC()
	err = j.Tx.InTx(ctx, j.Session, func(ctx context.Context, tx port.Tx) error {
		proposed, superseded = 0, 0
		models, err := j.Store.RouteModels(ctx, tx)
		if err != nil {
			return err
		}
		for _, m := range models {
			lp, ok := find(m.Endpoint.Provider, m.Model)
			if !ok {
				continue
			}
			want := [2]domain.Money{lp.Input, lp.Output}
			if m.Current != nil && m.Current[0].Cmp(want[0]) == 0 && m.Current[1].Cmp(want[1]) == 0 {
				continue
			}
			if m.PendingID != "" {
				if m.Pending[0].Cmp(want[0]) == 0 && m.Pending[1].Cmp(want[1]) == 0 {
					continue // already proposed
				}
				if err := j.Store.Supersede(ctx, tx, m.PendingID); err != nil {
					return err
				}
				superseded++
			}
			id, err := j.IDs.NewID("epp")
			if err != nil {
				return err
			}
			note := "listed price " + lp.Input.String() + "/" + lp.Output.String() + " USD per Mtok"
			if err := j.Store.Insert(ctx, tx, Proposal{ID: id.String(), EndpointID: m.Endpoint.ID, Model: m.Model,
				Input: lp.Input, Output: lp.Output, EffectiveFrom: now, Note: note}); err != nil {
				return err
			}
			proposed++
		}
		return nil
	})
	return proposed, superseded, err
}
