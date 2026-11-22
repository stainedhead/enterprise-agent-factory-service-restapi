//go:build load

// Package api holds the load tests of afs-api. Run them with `make load` and AFS_TEST_DATABASE_URL set.
package api

import (
	"context"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// RA-US-7.2 (M3-H4): a report over 90 days answers with p99 below one second. The data set is
// AFS_LOAD_TEAMS teams (default 20) of AFS_LOAD_AGENTS agents (default 10): 90 days of 5-minute
// activity per agent, hourly traffic, and daily cost rollups. The initial scale of the PRD is 200 teams
// and 2,000 agents; run with AFS_LOAD_TEAMS=200 for it. Team and agent reports of every kind and the
// service usage and cost reports must meet the bound. Service activity and traffic reports read the
// whole 5-minute table (the daily views are plain views over it); their timings are logged, and
// research X-DB-8 asks -db for a rollup table.
func TestReportsP99Over90Days(t *testing.T) {
	db := pgtest.Get(t)
	teams, perTeam := envInt("AFS_LOAD_TEAMS", 20), envInt("AFS_LOAD_AGENTS", 10)
	admin := db.Admin(t)
	now := time.Now().UTC().Truncate(5 * time.Minute)
	from := now.AddDate(0, 0, -91)
	ctx := t.Context()
	var teamIDs []string
	var firstAgent string
	for range teams {
		tm := db.SeedTeam(t)
		teamIDs = append(teamIDs, tm.ID)
		agents := make([]string, perTeam)
		for i := range agents {
			agents[i] = pgtest.NewID("agt")
		}
		if firstAgent == "" {
			firstAgent = agents[0]
		}
		must := func(sql string, args ...any) {
			if _, err := admin.Exec(ctx, sql, args...); err != nil {
				t.Fatal(err)
			}
		}
		must(`INSERT INTO afs.agent_activity (bucket_start, team_id, agent_id, state, active_seconds, model_calls)
SELECT b, $1, a, w.s, CASE WHEN w.s = 'working' THEN 120 ELSE 0 END, CASE WHEN w.s = 'working' THEN 2 ELSE 0 END
FROM generate_series($2::timestamptz, $3::timestamptz, interval '5 minutes') b, unnest($4::text[]) a,
LATERAL (SELECT CASE WHEN (extract(epoch FROM b)::bigint / 300 + length(a) + ascii(right(a, 1))) % 3 = 0 THEN 'working' ELSE 'idle' END AS s) w`,
			tm.ID, from, now, agents)
		must(`INSERT INTO afs.external_traffic (bucket_start, team_id, agent_id, host, port, verdict, denied_reason, connections, bytes_out, bytes_in)
SELECT b, $1, a, h.host, 443, h.v, CASE WHEN h.v = 'denied' THEN 'not_in_catalog' END, 3, 300, 3000
FROM generate_series($2::timestamptz, $3::timestamptz, interval '1 hour') b, unnest($4::text[]) a,
(VALUES ('api.example.org', 'allowed'), ('evil.example.net', 'denied')) AS h(host, v)`, tm.ID, from, now, agents)
		must(`INSERT INTO afs.cost_daily (team_id, day, source, agent_id, model_id, calls, input_tokens, output_tokens, cost_usd)
SELECT $1, d::date, 'gateway', a, m, 100, 10000, 2000, 1.25
FROM generate_series($2::date, $3::date, interval '1 day') d, unnest($4::text[]) a, unnest(ARRAY['model-x', 'model-y']) m`, tm.ID, from, now, agents)
	}
	must := func(sql string) {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	must(`ANALYZE afs.agent_activity`)
	must(`ANALYZE afs.external_traffic`)
	must(`ANALYZE afs.cost_daily`)

	pool, err := postgres.OpenPool(ctx, postgres.Config{URL: db.URLs["afs_app"], Service: "afs-api", MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	runner := postgres.NewTxRunner(pool)
	repo := postgres.NewReportsRepo()
	team := teamIDs[0]
	viewer := port.Session{Actor: "usr_01JD3Z5Y8K2M4N6P8Q0R2S4T6V", ActorType: port.ActorUser, TeamIDs: []string{team}}
	service := port.Session{Actor: "usr_01JD3Z5Y8K2M4N6P8Q0R2S4T6V", ActorType: port.ActorUser, ServiceAdmin: true}

	type probe struct {
		name    string
		session port.Session
		kind    domain.ReportKind
		raw     domain.RawReportQuery
		gate    bool
	}
	r90 := func(groupBy string, f func(*domain.RawReportQuery)) domain.RawReportQuery {
		raw := domain.RawReportQuery{Range: "90d", GroupBy: groupBy}
		if f != nil {
			f(&raw)
		}
		return raw
	}
	var probes []probe
	for _, k := range []domain.ReportKind{domain.ReportKindUsage, domain.ReportKindCosts, domain.ReportKindActivity, domain.ReportKindTraffic} {
		probes = append(probes,
			probe{"team " + string(k) + " by day", viewer, k, r90("day", nil), true},
			probe{"team " + string(k) + " by agent", viewer, k, r90("agent", nil), true},
			probe{"agent " + string(k), viewer, k, r90("day", func(r *domain.RawReportQuery) { r.AgentID = firstAgent }), true})
	}
	probes = append(probes,
		probe{"service usage by team", service, domain.ReportKindUsage, r90("team", nil), true},
		probe{"service costs by day", service, domain.ReportKindCosts, r90("day", nil), true},
		probe{"service activity by team", service, domain.ReportKindActivity, r90("team", nil), false},
		probe{"service traffic by system", service, domain.ReportKindTraffic, r90("system", nil), false})

	for _, p := range probes {
		q, err := domain.ParseReportQuery(p.kind, p.raw, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if p.session.ServiceAdmin == false {
			q.Filters.TeamID = team
		}
		var times []time.Duration
		for range 30 {
			start := time.Now()
			if err := runner.ReadTx(ctx, p.session, func(ctx context.Context, tx port.Tx) error {
				_, err := repo.Run(ctx, tx, q, 0, 51)
				return err
			}); err != nil {
				t.Fatalf("%s: %v", p.name, err)
			}
			times = append(times, time.Since(start))
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		p99 := times[len(times)-1]
		t.Logf("%-32s p50 %-10v p99 %-10v (%d teams x %d agents)", p.name, times[len(times)/2], p99, teams, perTeam)
		if p.gate && p99 >= time.Second {
			t.Errorf("%s: p99 %v is not below 1 s", p.name, p99)
		}
	}
}
