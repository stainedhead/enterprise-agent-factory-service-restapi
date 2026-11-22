package gateway_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

func TestOutputCap(t *testing.T) {
	cases := []struct{ requested, model, want int }{
		{0, 4096, 4096}, {100, 4096, 100}, {4096, 4096, 4096}, {5000, 4096, 4096}, {0, 0, gateway.DefaultMaxOutputTokens}, {500, 0, 500},
	}
	for _, tc := range cases {
		if got := gateway.OutputCap(tc.requested, tc.model); got != tc.want {
			t.Errorf("OutputCap(%d, %d) = %d, want %d", tc.requested, tc.model, got, tc.want)
		}
	}
}

func TestInputEstimateIsBytesOverThreeRoundedUp(t *testing.T) {
	for in, want := range map[int]int{0: 0, 1: 1, 3: 1, 4: 2, 300: 100, 301: 101} {
		if got := gateway.InputEstimate(in); got != want {
			t.Errorf("InputEstimate(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestReserveAmount(t *testing.T) {
	p := gateway.Price{Input: domain.MustMoney("3"), Output: domain.MustMoney("15")}
	cases := []struct {
		name       string
		cap, bytes int
		price      gateway.Price
		want       string
	}{
		{"no maxTokens: the model's max", 4096, 0, p, "0.06144"},
		{"with input", 1000, 3000, p, "0.018"},
		{"rounds up", 1, 1, p, "0.000018"},
		{"free model still reserves a micro-dollar", 10, 10, gateway.Price{}, "0.000001"},
		{"huge values do not overflow", 10_000_000, 20 << 20, gateway.Price{Input: domain.MustMoney("999999.999999"), Output: domain.MustMoney("999999.999999")}, "10000000.00000"},
	}
	for _, tc := range cases {
		got := gateway.ReserveAmount(tc.price, tc.cap, tc.bytes)
		if tc.name == "huge values do not overflow" {
			if got.IsNegative() {
				t.Errorf("%s: negative %s", tc.name, got)
			}
			continue
		}
		if got.String() != domain.MustMoney(tc.want).String() {
			t.Errorf("%s: reserved %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestActualCostCountsCacheTokens(t *testing.T) {
	p := gateway.Price{Input: domain.MustMoney("3"), Output: domain.MustMoney("15"), CacheRead: domain.MustMoney("0.3"), CacheWrite: domain.MustMoney("3.75")}
	if got := gateway.ActualCost(p, gateway.Usage{Input: 1000, Output: 500, CacheRead: 2000, CacheWrite: 100}).String(); got != "0.011475" {
		t.Fatalf("cost = %s", got)
	}
	if got := gateway.ActualCost(p, gateway.Usage{}).Micros(); got != 0 {
		t.Fatalf("cost of nothing = %d", got)
	}
}

func TestEffectivePrice(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	rows := []gateway.Price{{EffectiveFrom: now.Add(time.Hour), Input: domain.MustMoney("9")}, {EffectiveFrom: now.Add(-time.Hour), Input: domain.MustMoney("3")}, {EffectiveFrom: now.Add(-time.Hour * 48), Input: domain.MustMoney("1")}}
	p, ok := gateway.EffectivePrice(rows, now)
	if !ok || p.Input.String() != "3.00" {
		t.Fatalf("price = %+v ok %v: a price not yet effective must not apply", p, ok)
	}
	if _, ok := gateway.EffectivePrice(rows[:1], now); ok {
		t.Fatal("a price from the future was used")
	}
}

func TestBuildMetadata(t *testing.T) {
	c := gateway.Caller{TeamID: "team_1", AgentID: "agt_1"}
	var tags []gateway.Tag
	for i := range 20 {
		tags = append(tags, gateway.Tag{Key: fmt.Sprintf("team-%02d", i), Value: "v", Source: "team"})
	}
	tags = append(tags, gateway.Tag{Key: "zz-service", Value: "s", Source: "service"}, gateway.Tag{Key: "empty", Value: "", Source: "team"})
	meta, dropped := gateway.BuildMetadata(c, tags)
	if len(meta) != 16 {
		t.Fatalf("pairs = %d, want 16", len(meta))
	}
	if meta["afs-team-id"] != "team_1" || meta["afs-agent-id"] != "agt_1" || meta["zz-service"] != "s" {
		t.Fatalf("system and service tags come first: %v", meta)
	}
	if meta["team-00"] != "v" || meta["team-12"] != "v" || meta["team-13"] != "" {
		t.Fatalf("team tags are kept in key order until 16: %v", meta)
	}
	if len(dropped) != 7 || dropped[0] != "team-13" {
		t.Fatalf("dropped = %v", dropped)
	}
	pool, _ := gateway.BuildMetadata(gateway.Caller{TeamID: "team_1", PoolID: "pool_1"}, nil)
	if pool["afs-pool-id"] != "pool_1" || pool["afs-agent-id"] != "" {
		t.Fatalf("pool metadata = %v", pool)
	}
}

func TestStreamMeter(t *testing.T) {
	var m gateway.StreamMeter
	if r := m.Result(); r.Started || r.UsageReported {
		t.Fatalf("empty meter = %+v", r)
	}
	m.Started()
	if r := m.Result(); !r.Started || r.UsageReported {
		t.Fatalf("started = %+v", r)
	}
	m.Observe(gateway.Usage{Input: 12, Output: 1, CacheRead: gateway.Absent, CacheWrite: gateway.Absent})
	m.Observe(gateway.Usage{Input: gateway.Absent, Output: 30, CacheRead: gateway.Absent, CacheWrite: gateway.Absent})
	r := m.Result()
	if !r.UsageReported || r.Usage.Input != 12 || r.Usage.Output != 30 {
		t.Fatalf("merged = %+v", r)
	}
}

func TestPeriodStartAndPercent(t *testing.T) {
	now := time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC)
	if got := gateway.PeriodStart(gateway.BudgetRecord{Period: "monthly"}, now); !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly = %v", got)
	}
	if got := gateway.Percent(domain.MustMoney("8"), domain.MustMoney("10")); got != 80 {
		t.Fatalf("percent = %d", got)
	}
	if got := gateway.Percent(domain.MustMoney("1"), domain.MustMoney("0")); got != 0 {
		t.Fatalf("percent of a zero limit = %d", got)
	}
	if !gateway.ValidModelID("anthropic.claude-3-5-sonnet-20241022-v2:0") || gateway.ValidModelID("bad id") || gateway.ValidModelID("") {
		t.Fatal("ValidModelID")
	}
}
