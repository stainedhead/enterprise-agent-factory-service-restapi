package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/costtags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/costtags/costtagstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

// BI-6: the daily tag check as afs_billing against a real database: the keys come from the service and
// team tags, "seen" from the CUR rows' tag sets, the status row is upserted and one event is raised.
func TestCostTagCheckAsBilling(t *testing.T) {
	g := newBillingRigAs(t, "afs_billing")
	team := g.db.SeedTeam(t)
	seenKey, quietKey := "bill-key-seen-"+team.Slug, "bill-key-quiet-"+team.Slug
	t.Cleanup(func() {
		_, _ = g.admin.Exec(context.Background(), `DELETE FROM afs.cost_allocation_tag_status`)
		_, _ = g.admin.Exec(context.Background(), `DELETE FROM afs.outbox_events WHERE event_type = 'cost_tags.not_seen'`)
	})
	for _, k := range []string{seenKey, quietKey} {
		g.exec(`INSERT INTO afs.service_tags (key, mode, value, description) VALUES ($1, 'fixed', 'v', '')`, k)
	}
	day := afsDay(5)
	if err := g.store.ReplaceDay(t.Context(), day, []cur.Row{
		{TeamID: team.ID, Source: cur.SourceBedrock, ModelID: "m1", Cost: domain.MustMoney("1"), Tags: map[string]string{seenKey: "v"}},
		{TeamID: team.ID, Source: cur.SourceEKS, PoolID: "pool_1", Cost: domain.MustMoney("2")},
	}); err != nil {
		t.Fatal(err)
	}

	since := time.Now().UTC().Add(-5 * 24 * time.Hour).Truncate(time.Second)
	b := &costtagstest.Billing{Tags: map[string]costtags.Activation{
		seenKey: {Active: true, Since: since}, quietKey: {Active: true, Since: since}, "afs-pool-id": {Active: true, Since: since},
	}}
	clk := &fakes.Clock{T: time.Now().UTC()}
	c := &costtags.Checker{Billing: b, Store: postgres.NewCostTagsStore(), Tx: g.runner, Outbox: postgres.NewOutboxWriter(),
		Clock: clk, IDs: billingIDs{}, Session: postgres.BillingSession()}

	for i := range 2 { // the rerun changes nothing and raises nothing
		res, err := c.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if want := 1 - i; !containsAll(res.NotSeen, quietKey) && want == 1 {
			t.Fatalf("run %d: not seen = %v", i, res.NotSeen)
		}
		if i == 1 && len(res.NotSeen) != 0 {
			t.Fatalf("rerun raised %v", res.NotSeen)
		}
	}
	row := func(k string) string {
		return g.scalar(`SELECT seen_in_cur::text || '/' || aws_status || '/' || coalesce(first_seen_at::date::text, '-') || '/' || coalesce(last_seen_at::date::text, '-')
FROM afs.cost_allocation_tag_status WHERE key = $1`, k)
	}
	if v := row(seenKey); v != "true/active/2031-03-05/2031-03-05" {
		t.Errorf("seen key row = %s", v)
	}
	if v := row(quietKey); v != "false/active/-/-" {
		t.Errorf("quiet key row = %s", v)
	}
	if v := row("afs-pool-id"); v[:4] != "true" {
		t.Errorf("pool key is evidenced by the pool row: %s", v)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.outbox_events WHERE event_type = 'cost_tags.not_seen' AND aggregate_id = $1`, quietKey); n != "1" {
		t.Errorf("events for the quiet key = %s", n)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.outbox_events WHERE event_type = 'cost_tags.not_seen' AND aggregate_id = $1`, seenKey); n != "0" {
		t.Errorf("events for the seen key = %s", n)
	}
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
