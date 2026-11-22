package domain_test

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

var budgetNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) // day 16 of a 30-day month, 15.5 days in

func ceilingTeam(kind domain.TeamKind) domain.BudgetTeam {
	return domain.BudgetTeam{Kind: kind, MaxBudget: domain.MustMoney("2000")}
}

func put(amount string) domain.PutBudgetParams {
	return domain.PutBudgetParams{Period: domain.BudgetPeriodMonthly, Amount: domain.MustMoney(amount)}
}

func TestEffectiveCeiling(t *testing.T) {
	over := domain.MustMoney("5000")
	future, past := budgetNow.Add(time.Hour), budgetNow.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		tm   domain.BudgetTeam
		want string
	}{
		{"base", ceilingTeam(domain.TeamKindPersistent), "2000.00"},
		{"unexpired override", domain.BudgetTeam{MaxBudget: domain.MustMoney("2000"), CeilingOverride: &over, CeilingOverrideExpiresAt: &future}, "5000.00"},
		{"expired override", domain.BudgetTeam{MaxBudget: domain.MustMoney("2000"), CeilingOverride: &over, CeilingOverrideExpiresAt: &past}, "2000.00"},
		{"override without expiry is ignored", domain.BudgetTeam{MaxBudget: domain.MustMoney("2000"), CeilingOverride: &over}, "2000.00"},
	} {
		if got := tc.tm.EffectiveCeiling(budgetNow).String(); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestNewBudget(t *testing.T) {
	over := domain.MustMoney("5000")
	future := budgetNow.Add(24 * time.Hour)
	withOverride := domain.BudgetTeam{Kind: domain.TeamKindPersistent, MaxBudget: domain.MustMoney("2000"), CeilingOverride: &over, CeilingOverrideExpiresAt: &future}
	notify := put("100")
	notify.OnExhaust = domain.OnExhaustNotify
	lifetime := put("100")
	lifetime.Period = domain.BudgetPeriodLifetime
	past := put("100")
	past.ExpiresAt = &budgetNow
	badTh := put("100")
	badTh.Thresholds = []int{50, 33}
	dupTh := put("100")
	dupTh.Thresholds = []int{80, 50, 50}
	cents := put("100.005")
	huge := put("10000000000")
	huge.Amount = domain.MustMoney("10000000000")
	badPeriod := put("100")
	badPeriod.Period = "weekly"
	badExhaust := put("100")
	badExhaust.OnExhaust = "ignore"
	ephemeralMonthly := put("100")

	for _, tc := range []struct {
		name  string
		tm    domain.BudgetTeam
		p     domain.PutBudgetParams
		field string // "" means valid
	}{
		{"at the ceiling", ceilingTeam(domain.TeamKindPersistent), put("2000"), ""},
		{"above the ceiling", ceilingTeam(domain.TeamKindPersistent), put("2000.01"), "amountUsd"},
		{"up to an unexpired override", withOverride, put("5000"), ""},
		{"above an unexpired override", withOverride, put("5000.01"), "amountUsd"},
		{"zero", ceilingTeam(domain.TeamKindPersistent), put("0"), "amountUsd"},
		{"more than cents", ceilingTeam(domain.TeamKindPersistent), cents, "amountUsd"},
		{"beyond numeric(12,2)", domain.BudgetTeam{MaxBudget: domain.MustMoney("99999999999")}, huge, "amountUsd"},
		{"notify on persistent", ceilingTeam(domain.TeamKindPersistent), notify, ""},
		{"notify on ephemeral", ceilingTeam(domain.TeamKindEphemeral), notify, "onExhaust"},
		{"monthly on ephemeral", ceilingTeam(domain.TeamKindEphemeral), ephemeralMonthly, "period"},
		{"lifetime on ephemeral", ceilingTeam(domain.TeamKindEphemeral), lifetime, ""},
		{"expiry not in the future", ceilingTeam(domain.TeamKindPersistent), past, "expiresAt"},
		{"threshold not allowed", ceilingTeam(domain.TeamKindPersistent), badTh, "thresholds"},
		{"duplicate thresholds", ceilingTeam(domain.TeamKindPersistent), dupTh, "thresholds"},
		{"unknown period", ceilingTeam(domain.TeamKindPersistent), badPeriod, "period"},
		{"unknown onExhaust", ceilingTeam(domain.TeamKindPersistent), badExhaust, "onExhaust"},
	} {
		b, err := domain.NewBudget("bud_01JD2Q3R4S5T6V7W8X9Y0Z0001", "team_x", tc.p, tc.tm, budgetNow)
		if tc.field == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			} else if b.Version != 1 || b.StartsAt != budgetNow {
				t.Errorf("%s: %+v", tc.name, b)
			}
			continue
		}
		fe := domain.FieldErrors(err)
		if domain.KindOf(err) != domain.KindValidation || len(fe) == 0 || fe[0].Field != tc.field {
			t.Errorf("%s: want a validation error on %s, got %v", tc.name, tc.field, err)
		}
	}
}

func TestNewBudgetDefaults(t *testing.T) {
	b, err := domain.NewBudget("bud_01JD2Q3R4S5T6V7W8X9Y0Z0001", "team_x", put("100"), ceilingTeam(domain.TeamKindPersistent), budgetNow)
	if err != nil {
		t.Fatal(err)
	}
	if b.OnExhaust != domain.OnExhaustBlock || len(b.Thresholds) != 3 || b.Thresholds[0] != 50 || b.Thresholds[2] != 100 {
		t.Fatalf("defaults: %+v", b)
	}
	p := put("100")
	p.Thresholds = []int{90, 50, 75}
	b, _ = domain.NewBudget("bud_01JD2Q3R4S5T6V7W8X9Y0Z0001", "team_x", p, ceilingTeam(domain.TeamKindPersistent), budgetNow)
	if b.Thresholds[0] != 50 || b.Thresholds[1] != 75 || b.Thresholds[2] != 90 {
		t.Fatalf("thresholds are sorted: %v", b.Thresholds)
	}
}

func TestReplaceKeepsIdentity(t *testing.T) {
	cur, _ := domain.NewBudget("bud_01JD2Q3R4S5T6V7W8X9Y0Z0001", "team_x", put("100"), ceilingTeam(domain.TeamKindPersistent), budgetNow.Add(-48*time.Hour))
	cur.Version, cur.CreatedBy = 4, "usr_a"
	p := put("250")
	p.Period = domain.BudgetPeriodLifetime
	next, err := cur.Replace(p, ceilingTeam(domain.TeamKindPersistent), budgetNow)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != cur.ID || next.StartsAt != cur.StartsAt || next.CreatedBy != "usr_a" || next.Version != 4 ||
		next.Amount.String() != "250.00" || next.Period != domain.BudgetPeriodLifetime {
		t.Fatalf("%+v", next)
	}
	// An ephemeral team's lifetime budget cannot be turned into a monthly one (RA-US-10.2).
	eph, _ := domain.NewBudget("bud_01JD2Q3R4S5T6V7W8X9Y0Z0002", "team_y", func() domain.PutBudgetParams {
		q := put("100")
		q.Period = domain.BudgetPeriodLifetime
		return q
	}(), ceilingTeam(domain.TeamKindEphemeral), budgetNow)
	if _, err := eph.Replace(put("100"), ceilingTeam(domain.TeamKindEphemeral), budgetNow); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("ephemeral to monthly: %v", err)
	}
}

func TestPeriod(t *testing.T) {
	monthly := domain.Budget{Period: domain.BudgetPeriodMonthly}
	s, e := monthly.CurrentPeriod(budgetNow)
	if !s.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || e == nil || !e.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly: %v %v", s, e)
	}
	// A local-zone instant still maps to the UTC month.
	loc := time.FixedZone("x", -8*3600)
	s, _ = monthly.CurrentPeriod(time.Date(2026, 11, 30, 20, 0, 0, 0, loc)) // 2026-12-01 04:00 UTC
	if !s.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("UTC month: %v", s)
	}
	start := time.Date(2026, 10, 3, 8, 30, 0, 0, time.UTC)
	exp := start.Add(90 * 24 * time.Hour)
	lt := domain.Budget{Period: domain.BudgetPeriodLifetime, StartsAt: start, ExpiresAt: &exp}
	s, e = lt.CurrentPeriod(budgetNow)
	if !s.Equal(start) || e == nil || !e.Equal(exp) {
		t.Fatalf("lifetime: %v %v", s, e)
	}
	lt.ExpiresAt = nil
	if _, e = lt.CurrentPeriod(budgetNow); e != nil {
		t.Fatalf("lifetime without expiry has no end: %v", e)
	}
}

func TestPosition(t *testing.T) {
	m := domain.MustMoney
	monthly := domain.Budget{Period: domain.BudgetPeriodMonthly, Amount: m("2000")}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // budgetNow is 15.5 of 30 days in
	lifeStart := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	lifeEnd := lifeStart.Add(20 * 24 * time.Hour) // budgetNow is 10 of 20 days in
	life := domain.Budget{Period: domain.BudgetPeriodLifetime, Amount: m("1000"), StartsAt: lifeStart, ExpiresAt: &lifeEnd}
	open := domain.Budget{Period: domain.BudgetPeriodLifetime, Amount: m("1000"), StartsAt: lifeStart}
	at := func(t time.Time) *time.Time { return &t }
	for _, tc := range []struct {
		name     string
		b        domain.Budget
		spent    string
		reserved string
		pct      float64
		forecast string
		capAt    *time.Time
	}{
		// 812.40 in 15.5 of 30 days forecasts 1572.387097 (micro-dollars); under 2000, so no cap date.
		{"monthly under the cap", monthly, "812.40", "3.20", 40.6, "1572.387097", nil},
		// 1100 in 15.5 days: forecast 2129.03..., the 2000 cap is reached 28.2 days in.
		{"monthly over at month end", monthly, "1100", "0", 55, "2129.032258", at(start.Add(expectedOffset(m("1100"), m("2000"), 15.5*24)))},
		{"nothing spent", monthly, "0", "0", 0, "0.00", nil},
		{"lifetime with end, on pace", life, "500", "0", 50, "1000.00", at(lifeEnd)},
		{"lifetime with end, over pace", life, "700", "1.5", 70, "1400.00", at(lifeStart.Add(expectedOffset(m("700"), m("1000"), 240)))},
		{"lifetime without end forecasts the spend and dates the cap", open, "250", "0", 25, "250.00", at(lifeStart.Add(expectedOffset(m("250"), m("1000"), 240)))},
		{"exhausted", monthly, "2000", "0", 100, "3870.967742", at(start.Add(expectedOffset(m("2000"), m("2000"), 15.5*24)))},
	} {
		p := domain.NewBudgetPosition(tc.b, domain.SpendCounter{Spent: m(tc.spent), Reserved: m(tc.reserved)}, budgetNow)
		if p.Spent.String() != m(tc.spent).String() || p.Reserved.String() != m(tc.reserved).String() {
			t.Errorf("%s: spent/reserved %v %v", tc.name, p.Spent, p.Reserved)
		}
		if p.PercentUsed != tc.pct {
			t.Errorf("%s: percent %v, want %v", tc.name, p.PercentUsed, tc.pct)
		}
		if p.Forecast.String() != m(tc.forecast).String() {
			t.Errorf("%s: forecast %s, want %s", tc.name, p.Forecast, tc.forecast)
		}
		switch {
		case tc.capAt == nil && p.ProjectedCapAt != nil:
			t.Errorf("%s: projectedCapAt %v, want absent", tc.name, p.ProjectedCapAt)
		case tc.capAt != nil && (p.ProjectedCapAt == nil || p.ProjectedCapAt.Sub(*tc.capAt).Abs() > time.Second):
			t.Errorf("%s: projectedCapAt %v, want %v", tc.name, p.ProjectedCapAt, tc.capAt)
		}
	}
}

// expectedOffset is elapsed * amount / spent for an elapsed time of hours.
func expectedOffset(spent, amount domain.Money, hours float64) time.Duration {
	return time.Duration(hours * float64(time.Hour) * float64(amount.Micros()) / float64(spent.Micros()))
}

func TestPositionBeforeThePeriodHasElapsed(t *testing.T) {
	b := domain.Budget{Period: domain.BudgetPeriodMonthly, Amount: domain.MustMoney("100")}
	first := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := domain.NewBudgetPosition(b, domain.SpendCounter{Spent: domain.MustMoney("5")}, first)
	if p.Forecast.String() != "5.00" || p.ProjectedCapAt != nil {
		t.Fatalf("no time has elapsed, so nothing can be projected: %+v", p)
	}
	// A lifetime budget past its expiry forecasts the spend it has.
	start := first.Add(-48 * time.Hour)
	end := first.Add(-24 * time.Hour)
	p = domain.NewBudgetPosition(domain.Budget{Period: domain.BudgetPeriodLifetime, Amount: domain.MustMoney("100"), StartsAt: start, ExpiresAt: &end},
		domain.SpendCounter{Spent: domain.MustMoney("40")}, first)
	if p.Forecast.String() != "40.00" || p.ProjectedCapAt != nil {
		t.Fatalf("expired: %+v", p)
	}
}
