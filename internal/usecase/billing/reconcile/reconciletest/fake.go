// Package reconciletest is an in-memory reconcile.Repository. It derives the status the way
// afs.cost_reconciliation_status does, so a use case test sees the database's behavior.
package reconciletest

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/reconcile"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo is the store.
type Repo struct {
	// ThresholdPct is billing.variance_alert_pct (default 5 when zero).
	ThresholdPct int64
	// Present lists the days (YYYY-MM-DD) the CUR has rows for.
	Present map[string]bool
	// Gateway and Billed map day then team to the figures.
	Gateway map[string]map[string]domain.Money
	Billed  map[string]map[string]domain.Money
	// Rows are the stored reconciliations by "day/team".
	Rows map[string]reconcile.Reconciliation
	// Rollups counts RollupGateway calls per day.
	Rollups map[string]int
	// FailOn makes the named method ("Upsert", "Totals") fail.
	FailOn map[string]error
}

var _ reconcile.Repository = (*Repo)(nil)

// New returns an empty repo.
func New() *Repo {
	return &Repo{Present: map[string]bool{}, Gateway: map[string]map[string]domain.Money{}, Billed: map[string]map[string]domain.Money{},
		Rows: map[string]reconcile.Reconciliation{}, Rollups: map[string]int{}, FailOn: map[string]error{}}
}

// Set records a team's figures for a day and marks the CUR present.
func (r *Repo) Set(day, team, gateway, billed string) {
	if r.Gateway[day] == nil {
		r.Gateway[day], r.Billed[day] = map[string]domain.Money{}, map[string]domain.Money{}
	}
	r.Gateway[day][team] = domain.MustMoney(gateway)
	r.Billed[day][team] = domain.MustMoney(billed)
	r.Present[day] = true
}

// BillingPresent implements reconcile.Repository.
func (r *Repo) BillingPresent(_ context.Context, _ port.Tx, day time.Time) (bool, error) {
	return r.Present[day.Format(time.DateOnly)], nil
}

// RollupGateway implements reconcile.Repository.
func (r *Repo) RollupGateway(_ context.Context, _ port.Tx, day time.Time) error {
	r.Rollups[day.Format(time.DateOnly)]++
	return nil
}

// Totals implements reconcile.Repository.
func (r *Repo) Totals(_ context.Context, _ port.Tx, day time.Time) ([]reconcile.TeamDay, error) {
	if err := r.FailOn["Totals"]; err != nil {
		return nil, err
	}
	k := day.Format(time.DateOnly)
	teams := map[string]bool{}
	for t := range r.Gateway[k] {
		teams[t] = true
	}
	for t := range r.Billed[k] {
		teams[t] = true
	}
	var out []reconcile.TeamDay
	for t := range teams {
		out = append(out, reconcile.TeamDay{TeamID: t, GatewayUSD: r.Gateway[k][t], BilledUSD: r.Billed[k][t]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TeamID < out[j].TeamID })
	return out, nil
}

// Previous implements reconcile.Repository.
func (r *Repo) Previous(_ context.Context, _ port.Tx, team string, day time.Time) (string, bool, error) {
	row, ok := r.Rows[day.Format(time.DateOnly)+"/"+team]
	return row.Status, ok, nil
}

// Upsert implements reconcile.Repository.
func (r *Repo) Upsert(_ context.Context, _ port.Tx, day time.Time, td reconcile.TeamDay) (reconcile.Reconciliation, error) {
	if err := r.FailOn["Upsert"]; err != nil {
		return reconcile.Reconciliation{}, err
	}
	th := r.ThresholdPct
	if th == 0 {
		th = 5
	}
	g, b := td.GatewayUSD.Micros(), td.BilledUSD.Micros()
	rec := reconcile.Reconciliation{TeamID: td.TeamID, Day: day, GatewayUSD: td.GatewayUSD, BilledUSD: td.BilledUSD}
	switch {
	case b == 0 && g == 0:
		rec.Status = reconcile.StatusOK
	case b == 0:
		rec.Status = reconcile.StatusMissing
	default:
		diff := g - b
		if diff < 0 {
			diff = -diff
		}
		rec.VariancePct = fmt.Sprintf("%.3f", float64(g-b)/float64(b)*100)
		rec.Status = reconcile.StatusOK
		if float64(diff)/float64(b)*100 > float64(th) {
			rec.Status = reconcile.StatusOverThreshold
		}
	}
	r.Rows[day.Format(time.DateOnly)+"/"+td.TeamID] = rec
	return rec, nil
}
