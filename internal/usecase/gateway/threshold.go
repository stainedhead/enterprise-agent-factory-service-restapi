package gateway

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// crossedSet remembers the crossings this process already recorded, so 200 concurrent callers cost
// one database insert. The database row (insert-once) stays the truth across replicas.
type crossedSet struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func (c *crossedSet) add(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		return false
	}
	if c.m == nil || len(c.m) > 50_000 {
		c.m = map[string]struct{}{}
	}
	c.m[key] = struct{}{}
	return true
}

func (c *crossedSet) forget(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

func crossKey(budgetID string, period time.Time, threshold int) string {
	return budgetID + "|" + period.UTC().Format(time.RFC3339) + "|" + itoa(threshold)
}

// Percent is spent as a whole percentage of limit, rounded down.
func Percent(spent, limit domain.Money) int {
	if limit.Micros() <= 0 {
		return 0
	}
	return int(spent.Micros() * 100 / limit.Micros())
}

// crossedThresholds returns the budget's thresholds that the counter has reached, ascending.
func crossedThresholds(b BudgetRecord, c Counter) []int {
	pct := Percent(c.Spent, c.Limit)
	var out []int
	for _, t := range b.Thresholds {
		if t <= pct {
			out = append(out, t)
		}
	}
	sort.Ints(out)
	return out
}

// recordThresholds writes one budget.threshold_crossed per reached threshold, once per budget period
// (GW-4). It runs after the response, so a failure is logged and retried by the next call.
func (s *Service) recordThresholds(ctx context.Context, c Caller, b BudgetRecord, period time.Time, ctr Counter) {
	for _, t := range crossedThresholds(b, ctr) {
		key := crossKey(b.ID, period, t)
		if !s.crossed.add(key) {
			continue
		}
		cr := Crossing{TeamID: c.TeamID, BudgetID: b.ID, Period: period, Threshold: t, Event: domain.EventBudgetThresholdCrossed,
			Data: map[string]any{"budgetId": b.ID, "period": period.UTC().Format(time.RFC3339), "threshold": t,
				"spentUsd": ctr.Spent.String(), "limitUsd": ctr.Limit.String()}}
		if _, err := s.thresholds.Record(ctx, cr); err != nil {
			s.crossed.forget(key)
			s.log.Error("record threshold", "budgetId", b.ID, "threshold", t, "err", err)
		}
	}
}

// recordExhausted writes the one budget.exhausted of a period, on the first refusal at 100%.
func (s *Service) recordExhausted(ctx context.Context, c Caller, b BudgetRecord, period time.Time, h Hold) {
	key := crossKey(b.ID, period, ExhaustedMarker)
	if !s.crossed.add(key) {
		return
	}
	cr := Crossing{TeamID: c.TeamID, BudgetID: b.ID, Period: period, Threshold: ExhaustedMarker, Event: domain.EventBudgetExhausted,
		Data: map[string]any{"budgetId": b.ID, "period": period.UTC().Format(time.RFC3339),
			"spentUsd": h.Spent.String(), "limitUsd": h.Limit.String(), "onExhaust": b.OnExhaust}}
	if _, err := s.thresholds.Record(ctx, cr); err != nil {
		s.crossed.forget(key)
		s.log.Error("record exhausted", "budgetId", b.ID, "err", err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
