package domain

import (
	"math/big"
	"slices"
	"time"
)

// AllowedBudgetThresholds are the percentages a budget may notify at (database check on
// afs.budgets.thresholds).
var AllowedBudgetThresholds = []int{1, 5, 10, 20, 25, 30, 40, 50, 60, 70, 75, 80, 85, 90, 95, 100}

// DefaultBudgetThresholds are the thresholds of a budget that names none.
var DefaultBudgetThresholds = []int{50, 80, 100}

// maxBudgetMicros is the largest amount afs.budgets.amount_usd (numeric(12,2)) holds: 9,999,999,999.99 USD.
const maxBudgetMicros = 9_999_999_999_990_000

// Budget is a team's budget (API contract §5): one per team, monthly or lifetime. It is desired
// state; the gateway meters against the spend counter of its current period.
type Budget struct {
	ID         string
	TeamID     string
	Period     BudgetPeriod
	Amount     Money
	Thresholds []int
	OnExhaust  OnExhaust
	StartsAt   time.Time
	ExpiresAt  *time.Time
	Version    Version
	CreatedAt  time.Time
	CreatedBy  string
	UpdatedAt  time.Time
}

// BudgetTeam is what a budget rule needs to know about its team: kind and the ceiling in force.
type BudgetTeam struct {
	Kind                     TeamKind
	MaxBudget                Money
	CeilingOverride          *Money
	CeilingOverrideExpiresAt *time.Time
}

// EffectiveCeiling is the ceiling a budget may reach at now: the override while it is unexpired,
// else maxBudgetUsd (API contract §4.2, D34).
func (t BudgetTeam) EffectiveCeiling(now time.Time) Money {
	if t.CeilingOverride != nil && t.CeilingOverrideExpiresAt != nil && t.CeilingOverrideExpiresAt.After(now) {
		return *t.CeilingOverride
	}
	return t.MaxBudget
}

// PutBudgetParams are the members of a PutBudget request. PUT replaces the budget: a member left out
// takes its default (the default thresholds, block on exhaust, no expiry).
type PutBudgetParams struct {
	Period     BudgetPeriod
	Amount     Money
	Thresholds []int
	OnExhaust  OnExhaust
	ExpiresAt  *time.Time
}

// RequestsNotify reports whether the request asks for notify-only exhaustion, which needs
// teams:set-guardrails (API contract §4.9, D33).
func (p PutBudgetParams) RequestsNotify() bool { return p.OnExhaust == OnExhaustNotify }

// validate checks p against the team and returns the normalized thresholds and exhaustion mode.
func (p PutBudgetParams) validate(team BudgetTeam, startsAt, now time.Time) ([]int, OnExhaust, error) {
	var fe []FieldError
	if !p.Period.Valid() {
		fe = append(fe, FieldError{Field: "period", Reason: "must be monthly or lifetime"})
	}
	switch {
	case p.Amount.Micros() <= 0:
		fe = append(fe, FieldError{Field: "amountUsd", Reason: "must be greater than zero"})
	case p.Amount.Micros()%10_000 != 0:
		fe = append(fe, FieldError{Field: "amountUsd", Reason: "must have at most two fraction digits"})
	case p.Amount.Micros() > maxBudgetMicros:
		fe = append(fe, FieldError{Field: "amountUsd", Reason: "is larger than a budget can hold"})
	default:
		if ceiling := team.EffectiveCeiling(now); p.Amount.Cmp(ceiling) > 0 {
			fe = append(fe, FieldError{Field: "amountUsd", Reason: "is above the team's budget ceiling of " + ceiling.String() + " USD"})
		}
	}
	on := p.OnExhaust
	if on == "" {
		on = OnExhaustBlock
	}
	if !on.Valid() {
		fe = append(fe, FieldError{Field: "onExhaust", Reason: "must be block or notify"})
	}
	if on == OnExhaustNotify && team.Kind == TeamKindEphemeral {
		fe = append(fe, FieldError{Field: "onExhaust", Reason: "an ephemeral team's budget always blocks at the cap"})
	}
	if team.Kind == TeamKindEphemeral && p.Period.Valid() && p.Period != BudgetPeriodLifetime {
		fe = append(fe, FieldError{Field: "period", Reason: "an ephemeral team's budget is a lifetime budget"})
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(now) {
		fe = append(fe, FieldError{Field: "expiresAt", Reason: "must be in the future"})
	} else if p.ExpiresAt != nil && !p.ExpiresAt.After(startsAt) {
		fe = append(fe, FieldError{Field: "expiresAt", Reason: "must be after the budget's start"})
	}
	th := slices.Clone(p.Thresholds)
	if th == nil {
		th = slices.Clone(DefaultBudgetThresholds)
	}
	slices.Sort(th)
	if len(th) == 0 || len(slices.Compact(slices.Clone(th))) != len(th) ||
		slices.ContainsFunc(th, func(v int) bool { return !slices.Contains(AllowedBudgetThresholds, v) }) {
		fe = append(fe, FieldError{Field: "thresholds", Reason: "must be distinct percentages from 1, 5, 10, 20, 25, 30, 40, 50, 60, 70, 75, 80, 85, 90, 95, 100"})
	}
	if len(fe) > 0 {
		return nil, "", Validation(fe...)
	}
	return th, on, nil
}

// NewBudget builds the first budget of a team in version 1.
func NewBudget(id, teamID string, p PutBudgetParams, team BudgetTeam, now time.Time) (Budget, error) {
	now = now.UTC()
	th, on, err := p.validate(team, now, now)
	if err != nil {
		return Budget{}, err
	}
	return Budget{ID: id, TeamID: teamID, Period: p.Period, Amount: p.Amount, Thresholds: th, OnExhaust: on,
		StartsAt: now, ExpiresAt: utcPtr(p.ExpiresAt), Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// Replace returns b replaced by p: identity, start, creator and version stay (the database bumps the
// version). A budget is never removed, so there is no way to drop an ephemeral team's lifetime
// budget (RA-US-10.2); the team rule in validate keeps its period a lifetime one.
func (b Budget) Replace(p PutBudgetParams, team BudgetTeam, now time.Time) (Budget, error) {
	now = now.UTC()
	th, on, err := p.validate(team, b.StartsAt, now)
	if err != nil {
		return Budget{}, err
	}
	b.Period, b.Amount, b.Thresholds, b.OnExhaust, b.ExpiresAt, b.UpdatedAt = p.Period, p.Amount, th, on, utcPtr(p.ExpiresAt), now
	return b, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// CurrentPeriod returns the period the budget is metered in at now: the UTC calendar month of a
// monthly budget, or from starts_at to the expiry (none when open-ended) of a lifetime budget. The
// start is the spend counter's period_start (database contract §8).
func (b Budget) CurrentPeriod(now time.Time) (start time.Time, end *time.Time) {
	if b.Period == BudgetPeriodLifetime {
		return b.StartsAt.UTC(), utcPtr(b.ExpiresAt)
	}
	n := now.UTC()
	start = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	e := start.AddDate(0, 1, 0)
	return start, &e
}

// SpendCounter is the spent and reserved amount of a budget's current period (zero when the period
// has no counter yet).
type SpendCounter struct {
	Spent    Money
	Reserved Money
}

// BudgetPosition is a budget with its current spend (API contract §5 Budget, RA-US-7.5).
type BudgetPosition struct {
	Budget
	PeriodStart time.Time
	Spent       Money
	Reserved    Money
	// PercentUsed is spent over the budget amount in percent, to one decimal.
	PercentUsed float64
	// Forecast is the spend at the end of the period if it continues at the rate so far (linear). It is
	// the spend so far when the period has no end, has ended or has not started.
	Forecast Money
	// ProjectedCapAt is when linear spend reaches the budget amount; nil when spend is zero or the
	// cap is not reached within the period.
	ProjectedCapAt *time.Time
}

// NewBudgetPosition computes the position of b at now from its counter.
func NewBudgetPosition(b Budget, c SpendCounter, now time.Time) BudgetPosition {
	now = now.UTC()
	start, end := b.CurrentPeriod(now)
	p := BudgetPosition{Budget: b, PeriodStart: start, Spent: c.Spent, Reserved: c.Reserved, Forecast: c.Spent}
	if b.Amount.Micros() > 0 {
		pct := new(big.Rat).Mul(big.NewRat(c.Spent.Micros(), b.Amount.Micros()), big.NewRat(1000, 1)) // tenths of a percent
		f, _ := pct.Float64()
		p.PercentUsed = float64(int64(f+0.5)) / 10
	}
	elapsed := now.Sub(start)
	if elapsed <= 0 || c.Spent.Micros() <= 0 {
		return p
	}
	if end != nil && now.Before(*end) {
		p.Forecast = scale(c.Spent, int64(end.Sub(start)), int64(elapsed))
	}
	// Linear spend reaches the amount after elapsed * amount / spent (the elapsed nanoseconds ride in a
	// Money, which is only an int64 here).
	toCap := scale(MoneyFromMicros(int64(elapsed)), b.Amount.Micros(), c.Spent.Micros())
	at := start.Add(time.Duration(toCap.Micros()))
	if end == nil || !at.After(*end) {
		p.ProjectedCapAt = &at
	}
	return p
}

// scale returns m * num / den rounded half up, without overflowing.
func scale(m Money, num, den int64) Money {
	r := new(big.Int).Mul(big.NewInt(m.Micros()), big.NewInt(num))
	r.Mul(r, big.NewInt(2))
	r.Add(r, big.NewInt(den))
	r.Div(r, big.NewInt(2*den))
	if !r.IsInt64() {
		return MoneyFromMicros(1<<63 - 1)
	}
	return MoneyFromMicros(r.Int64())
}
