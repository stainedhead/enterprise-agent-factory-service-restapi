package postgres_test

import (
	"encoding/csv"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/showback"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

// BI-US-5.1 / BI-US-6.1 against the database: the showback of a month has a row per cost center and per
// tag value under the tags in effect when each cost happened, an (untagged) row for rows without the
// key, and totals that equal cost_daily for the month, with spend outside the month left out.
func TestShowbackFromCostDaily(t *testing.T) {
	db := pgtest.Get(t)
	admin := db.Admin(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	exec(`UPDATE afs.teams SET cost_center = 'CC-SB-A' WHERE id = $1`, a.ID)
	exec(`UPDATE afs.teams SET cost_center = 'CC-SB-B' WHERE id = $1`, b.ID)
	tagSet := func(team, tags string) string {
		var id string
		if err := admin.QueryRow(t.Context(), `SELECT afs.tag_set_for($1, $2::jsonb)`, team, tags).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	payments, platform := tagSet(a.ID, `{"sb-unit":"payments"}`), tagSet(a.ID, `{"sb-unit":"platform"}`)
	other := tagSet(b.ID, `{"sb-other":"x"}`)
	cost := func(team, day, source, model string, calls, in, out int, usd string, ts any) {
		exec(`INSERT INTO afs.cost_daily (team_id, day, source, model_id, calls, input_tokens, output_tokens, cost_usd, tag_set_id)
VALUES ($1, $2::date, $3, $4, $5, $6, $7, $8::numeric, $9)`, team, day, source, model, calls, in, out, usd, ts)
	}
	// Monday under payments, Wednesday under platform (the tag changed in between), Tuesday split.
	cost(a.ID, "2026-10-05", "gateway", "m1", 10, 1000, 100, "1.250000", payments)
	cost(a.ID, "2026-10-05", "cur_bedrock", "m1", 0, 0, 0, "1.300000", payments)
	cost(a.ID, "2026-10-06", "gateway", "m1", 4, 400, 40, "0.500000", payments)
	cost(a.ID, "2026-10-06", "gateway", "m2", 6, 600, 60, "0.750000", platform)
	cost(a.ID, "2026-10-07", "gateway", "m1", 8, 800, 80, "1.000000", platform)
	cost(a.ID, "2026-10-07", "cur_eks", "", 0, 0, 0, "2.000000", platform)
	// Team B never carried the unit tag; one row has no tag set at all.
	cost(b.ID, "2026-10-08", "gateway", "m1", 3, 300, 30, "0.333333", other)
	cost(b.ID, "2026-10-09", "cur_other", "", 0, 0, 0, "4.000001", nil)
	// Outside the month: not counted.
	cost(a.ID, "2026-09-30", "gateway", "m1", 99, 9900, 990, "99.000000", payments)
	cost(a.ID, "2026-11-01", "gateway", "m1", 99, 9900, 990, "99.000000", payments)

	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_billing"], Service: "afs-billing", SystemActor: true, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	objs := &fakes.ObjectStore{}
	gen := &showback.Generator{Tx: postgres.NewTxRunner(pool), Source: postgres.NewShowbackSource(), Objects: objs, Clock: &fakes.Clock{T: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}}
	// The test database is shared with other tests' cost rows, so compare against cost_daily itself.
	res, err := gen.Run(t.Context(), time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), "sb-unit")
	if err != nil {
		t.Fatal(err)
	}
	body, err := objs.Body(res.Key)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	get := func(dim, value string) []string {
		t.Helper()
		for _, r := range rows {
			if r[0] == dim && r[1] == value {
				return r
			}
		}
		t.Fatalf("no row %s/%s in\n%s", dim, value, body)
		return nil
	}
	eq := func(r []string, want ...string) {
		t.Helper()
		if strings.Join(r[2:], ",") != strings.Join(want, ",") {
			t.Errorf("%s/%s = %v, want %v", r[0], r[1], r[2:], want)
		}
	}
	// calls, input, output, estimated, billed
	eq(get("costCenter", "CC-SB-A"), "28", "2800", "280", "3.50", "3.30")
	eq(get("costCenter", "CC-SB-B"), "3", "300", "30", "0.333333", "4.000001")
	eq(get("tag:sb-unit", "payments"), "14", "1400", "140", "1.75", "1.30")
	eq(get("tag:sb-unit", "platform"), "14", "1400", "140", "1.75", "2.00")
	untagged := get("tag:sb-unit", "(untagged)")
	if untagged[2] == "" {
		t.Error("no (untagged) row")
	}
	// The totals row of each dimension equals a direct sum of cost_daily for the month.
	var calls, in, out, est, billed string
	if err := admin.QueryRow(t.Context(), `SELECT coalesce(sum(calls) FILTER (WHERE source = 'gateway'), 0)::text, coalesce(sum(input_tokens) FILTER (WHERE source = 'gateway'), 0)::text,
coalesce(sum(output_tokens) FILTER (WHERE source = 'gateway'), 0)::text,
(coalesce(sum(cost_usd) FILTER (WHERE source = 'gateway'), 0) * 1000000)::bigint::text,
(coalesce(sum(cost_usd) FILTER (WHERE source <> 'gateway'), 0) * 1000000)::bigint::text
FROM afs.cost_daily WHERE day >= '2026-10-01' AND day <= '2026-10-31'`).Scan(&calls, &in, &out, &est, &billed); err != nil {
		t.Fatal(err)
	}
	usd := func(micros string) string {
		n, err := strconv.ParseInt(micros, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return domain.MoneyFromMicros(n).String()
	}
	est, billed = usd(est), usd(billed)
	eq(get("costCenter", "(total)"), calls, in, out, est, billed)
	eq(get("tag:sb-unit", "(total)"), calls, in, out, est, billed)
	// Team B's untagged rows are untagged for this key: its tag set has no sb-unit.
	if untagged[6] == "0.00" {
		t.Errorf("untagged billed = %s, want team B's CUR row and anything else without the key", untagged[6])
	}
}
