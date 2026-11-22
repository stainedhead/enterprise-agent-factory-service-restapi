// Package reconcile compares, per team and day, what the gateway estimated with what AWS billed for
// Bedrock (BI-2): it rolls the day's gateway usage into cost_daily, writes cost_reconciliations (the
// database derives the status from billing.variance_alert_pct) and publishes one
// cost.variance_detected when a team-day turns over_threshold.
//
// The gateway estimates only model calls, so the billed side is the cur_bedrock rows; EKS and storage
// cost have no gateway estimate and sit in cost_daily without a reconciliation.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Statuses of cost_reconciliations.
const (
	StatusOK            = "ok"
	StatusOverThreshold = "over_threshold"
	StatusMissing       = "missing_billing"
)

// TeamDay is the two figures of one team and day.
type TeamDay struct {
	TeamID     string
	GatewayUSD domain.Money
	// BilledUSD is the cur_bedrock cost.
	BilledUSD domain.Money
}

// Reconciliation is a stored cost_reconciliations row.
type Reconciliation struct {
	TeamID     string
	Day        time.Time
	GatewayUSD domain.Money
	BilledUSD  domain.Money
	// VariancePct is the database's figure, empty when billed is zero.
	VariancePct string
	Status      string
}

// Repository is the billing store, used inside a transaction.
type Repository interface {
	// BillingPresent reports whether any CUR row exists for the day. Until one does, billed zero would
	// make every metered day look unbilled.
	BillingPresent(ctx context.Context, tx port.Tx, day time.Time) (bool, error)
	// RollupGateway replaces the day's gateway rows of cost_daily with the sum of the day's usage events
	// (ok and error calls; rejected attempts are not calls).
	RollupGateway(ctx context.Context, tx port.Tx, day time.Time) error
	// Totals returns the gateway and billed Bedrock totals of every team with either.
	Totals(ctx context.Context, tx port.Tx, day time.Time) ([]TeamDay, error)
	// Previous returns the stored status of the team-day, false when none.
	Previous(ctx context.Context, tx port.Tx, teamID string, day time.Time) (string, bool, error)
	// Upsert writes the team-day and returns the row as the database derived it.
	Upsert(ctx context.Context, tx port.Tx, day time.Time, td TeamDay) (Reconciliation, error)
}

// Reconciler runs the reconciliation.
type Reconciler struct {
	Tx     port.TxRunner
	Outbox port.OutboxWriter
	Clock  port.Clock
	IDs    port.IDGen
	Repo   Repository
	Log    *slog.Logger
	// Session is the database session of the job (system:afs-billing).
	Session port.Session
	// OnVariance is called with the largest relative variance of the latest day reconciled
	// (abs(estimate - billed) / billed); nil ignores it.
	OnVariance func(ratio float64)
}

// DayResult is what one day did.
type DayResult struct {
	Day time.Time
	// Skipped is true when the CUR has nothing for the day yet.
	Skipped bool
	Teams   int
	// OverThreshold counts team-days over the threshold; Events counts the variance events published.
	OverThreshold int
	Events        int
	// MaxVarianceRatio is the largest relative variance of a team with billed cost.
	MaxVarianceRatio float64
}

// RunDay reconciles one UTC day in one transaction, so a row and its event commit together and a rerun
// publishes nothing new for a team-day that was already over the threshold.
func (r *Reconciler) RunDay(ctx context.Context, day time.Time) (DayResult, error) {
	day = day.UTC().Truncate(24 * time.Hour)
	var res DayResult
	err := r.Tx.InTx(ctx, r.Session, func(ctx context.Context, tx port.Tx) error {
		res = DayResult{Day: day}
		present, err := r.Repo.BillingPresent(ctx, tx, day)
		if err != nil {
			return err
		}
		if !present {
			res.Skipped = true
			return nil
		}
		if err := r.Repo.RollupGateway(ctx, tx, day); err != nil {
			return err
		}
		totals, err := r.Repo.Totals(ctx, tx, day)
		if err != nil {
			return err
		}
		for _, td := range totals {
			prev, found, err := r.Repo.Previous(ctx, tx, td.TeamID, day)
			if err != nil {
				return err
			}
			rec, err := r.Repo.Upsert(ctx, tx, day, td)
			if err != nil {
				return err
			}
			res.Teams++
			if billed := rec.BilledUSD.Micros(); billed > 0 {
				ratio := math.Abs(float64(rec.GatewayUSD.Micros()-billed)) / float64(billed)
				res.MaxVarianceRatio = math.Max(res.MaxVarianceRatio, ratio)
			}
			if rec.Status != StatusOverThreshold {
				continue
			}
			res.OverThreshold++
			if found && prev == StatusOverThreshold {
				continue // already announced for this team-day
			}
			if err := r.publish(ctx, tx, rec); err != nil {
				return err
			}
			res.Events++
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("reconcile %s: %w", day.Format(time.DateOnly), err)
	}
	if !res.Skipped && r.OnVariance != nil {
		r.OnVariance(res.MaxVarianceRatio)
	}
	return res, nil
}

func (r *Reconciler) publish(ctx context.Context, tx port.Tx, rec Reconciliation) error {
	id, err := r.IDs.NewID("evt")
	if err != nil {
		return err
	}
	data := map[string]any{
		"day": rec.Day.Format(time.DateOnly), "gatewayUsd": rec.GatewayUSD.String(), "billedUsd": rec.BilledUSD.String(),
		"variancePct": rec.VariancePct, "status": rec.Status,
	}
	ev, err := domain.NewEvent(domain.EventCostVarianceDetected, id.String(), r.Clock.Now(), r.Session.Actor, rec.TeamID,
		"cost_reconciliation", rec.TeamID+"/"+rec.Day.Format(time.DateOnly), data)
	if err != nil {
		return err
	}
	return r.Outbox.Publish(ctx, tx, ev)
}

// Run reconciles days in order and returns every day's result and the first error; a failed day does not
// stop the later ones.
func (r *Reconciler) Run(ctx context.Context, days []time.Time) ([]DayResult, error) {
	var out []DayResult
	var first error
	for _, d := range days {
		res, err := r.RunDay(ctx, d)
		out = append(out, res)
		if err != nil {
			if r.Log != nil {
				r.Log.Error("reconciliation failed", "day", d.Format(time.DateOnly), "err", err)
			}
			if first == nil {
				first = err
			}
			if ctx.Err() != nil {
				break
			}
		}
	}
	return out, first
}
