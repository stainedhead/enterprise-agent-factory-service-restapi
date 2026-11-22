package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reports"
)

// ReportsRepo implements reports.Repository (M3-H2). Usage and cost reports read the per-call table
// for ranges of two days or less and the daily rollup (cost_daily) above that. Activity and traffic
// reports read the 5-minute tables for ranges of two days or less and the daily rollups
// (afs.agent_activity_rollup_daily, afs.external_traffic_rollup_daily, -db 000018) above that, filled by the
// reconciler's rollup jobs (every 10 minutes for today and yesterday, nightly for a week), so a longer range
// lags the 5-minute tables by up to one job interval for today. Both paths sum to the same figures
// (RA-US-7.1). Every statement runs in the caller's transaction, so row-level security limits what is read.
type ReportsRepo struct{}

// NewReportsRepo returns a ReportsRepo.
func NewReportsRepo() *ReportsRepo { return &ReportsRepo{} }

var _ reports.Repository = (*ReportsRepo)(nil)

// TeamIsEphemeral implements reports.Repository.
func (*ReportsRepo) TeamIsEphemeral(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var kind string
	err = t.QueryRow(ctx, `SELECT kind FROM afs.teams WHERE id = $1`, teamID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, domain.NotFound("team")
	}
	if err != nil {
		return false, MapError(err, OpRead, "team")
	}
	return kind == "ephemeral", nil
}

// AgentExists implements reports.Repository. An agent that was deleted still has its history.
func (*ReportsRepo) AgentExists(ctx context.Context, tx port.Tx, teamID, agentID string) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	var one int
	err = t.QueryRow(ctx, `SELECT 1 FROM afs.agents WHERE id = $1 AND team_id = $2`, agentID, teamID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("agent")
	}
	return MapError(err, OpRead, "agent")
}

// reportSource describes one table or view a report reads.
type reportSource struct {
	from string
	// timeCond uses the placeholders {from} and {to} (timestamps) or {fromDay} and {toDay} (dates).
	timeCond string
	// dims maps a dimension to its SQL expression (a text); tagCol is the tag set column.
	dims   map[domain.ReportDim]string
	tagCol string
	// filters map a filter to its column ("" when the source has none).
	team, agent, pool, model, system string
	measures                         []string
}

const (
	dayText  = `to_char(%s, 'YYYY-MM-DD')`
	hourText = `to_char(date_trunc('hour', %s AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:00:00"Z"')`
	none     = `'(none)'`
)

func fiveMinuteDims(ts, team, agent, pool string) map[domain.ReportDim]string {
	return map[domain.ReportDim]string{
		domain.DimTeam:  team,
		domain.DimAgent: fmt.Sprintf(`coalesce(%s, %s)`, agent, none),
		domain.DimPool:  fmt.Sprintf(`coalesce(%s, %s)`, pool, none),
		domain.DimDay:   fmt.Sprintf(dayText, `(`+ts+` AT TIME ZONE 'UTC')::date`),
		domain.DimHour:  fmt.Sprintf(hourText, ts),
	}
}

func withDims(base map[domain.ReportDim]string, extra map[domain.ReportDim]string) map[domain.ReportDim]string {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

const (
	moneySum = `coalesce(sum(%s), 0)::text`
	utilExpr = `round(100.0 * sum(%[1]s) / nullif(sum(%[1]s) + sum(%[2]s), 0), 1)::float8`
)

func bigint(e string) string { return `(` + e + `)::bigint` }

// sourceFor picks the table for a query.
func sourceFor(q domain.ReportQuery) (reportSource, bool) {
	daily := q.Granularity == domain.Daily
	// baseRange is the time condition of a 5-minute table: the range itself for a short range, every
	// day touched for a long one.
	baseRange := func(col string) (string, bool) {
		if daily {
			return col + ` >= {fromDay}::timestamptz AND ` + col + ` < {toDay}::timestamptz + interval '24 hours'`, true
		}
		return col + ` >= {from} AND ` + col + ` < {to}`, false
	}
	switch q.Kind {
	case domain.ReportKindUsage, domain.ReportKindCosts:
		costs := q.Kind == domain.ReportKindCosts
		if !daily {
			s := reportSource{from: `afs.usage_events x`, timeCond: `x.occurred_at >= {from} AND x.occurred_at < {to}`,
				dims:   withDims(fiveMinuteDims(`x.occurred_at`, `x.team_id`, `x.agent_id`, `x.pool_id`), map[domain.ReportDim]string{domain.DimModel: `x.model_id`}),
				tagCol: `x.tag_set_id`, team: `x.team_id`, agent: `x.agent_id`, pool: `x.pool_id`, model: `x.model_id`}
			if costs {
				s.measures = []string{fmt.Sprintf(moneySum, `x.cost_usd`), `'0.00'`}
			} else {
				s.measures = []string{`count(*) FILTER (WHERE x.outcome IN ('ok', 'error'))::bigint`, bigint(`coalesce(sum(x.input_tokens), 0)`),
					bigint(`coalesce(sum(x.output_tokens), 0)`), fmt.Sprintf(moneySum, `x.cost_usd`)}
			}
			return s, true
		}
		s := reportSource{from: `afs.cost_daily x`, timeCond: `x.day >= {fromDay}::date AND x.day <= {toDay}::date`,
			dims: map[domain.ReportDim]string{
				domain.DimTeam: `x.team_id`, domain.DimAgent: fmt.Sprintf(`coalesce(x.agent_id, %s)`, none), domain.DimPool: fmt.Sprintf(`coalesce(x.pool_id, %s)`, none),
				domain.DimModel: fmt.Sprintf(`coalesce(x.model_id, %s)`, none), domain.DimDay: fmt.Sprintf(dayText, `x.day`)},
			tagCol: `x.tag_set_id`, team: `x.team_id`, agent: `x.agent_id`, pool: `x.pool_id`, model: `x.model_id`}
		if costs {
			s.measures = []string{`coalesce(sum(x.cost_usd) FILTER (WHERE x.source = 'gateway'), 0)::text`,
				`coalesce(sum(x.cost_usd) FILTER (WHERE x.source <> 'gateway'), 0)::text`}
		} else {
			s.timeCond += ` AND x.source = 'gateway'`
			s.measures = []string{bigint(`coalesce(sum(x.calls), 0)`), bigint(`coalesce(sum(x.input_tokens), 0)`),
				bigint(`coalesce(sum(x.output_tokens), 0)`), fmt.Sprintf(moneySum, `x.cost_usd`)}
		}
		return s, true
	case domain.ReportKindActivity:
		if daily {
			return rollupActivity(), true
		}
		w, i := `count(*) FILTER (WHERE x.state = 'working') * 300`, `count(*) FILTER (WHERE x.state = 'idle') * 300`
		cond, _ := baseRange(`x.bucket_start`)
		return reportSource{from: `afs.agent_activity x`, timeCond: cond,
			dims:   fiveMinuteDims(`x.bucket_start`, `x.team_id`, `x.agent_id`, `x.pool_id`),
			tagCol: `x.tag_set_id`, team: `x.team_id`, agent: `x.agent_id`, pool: `x.pool_id`,
			measures: []string{bigint(w), bigint(i), `(count(*) FILTER (WHERE x.state IN ('paused', 'stopped')) * 300)::bigint`,
				`(count(*) FILTER (WHERE x.state IN ('unhealthy', 'blocked_budget')) * 300)::bigint`, bigint(`coalesce(sum(x.model_calls), 0)`),
				`round(100.0 * count(*) FILTER (WHERE x.state = 'working') / nullif(count(*) FILTER (WHERE x.state IN ('working', 'idle')), 0), 1)::float8`}}, true
	case domain.ReportKindTraffic:
		if daily {
			return rollupTraffic(), true
		}
		sysDim := fmt.Sprintf(`coalesce(x.system_id, %s)`, none)
		cond, _ := baseRange(`x.bucket_start`)
		return reportSource{from: `afs.external_traffic x`, timeCond: cond,
			dims:   withDims(fiveMinuteDims(`x.bucket_start`, `x.team_id`, `x.agent_id`, `x.pool_id`), map[domain.ReportDim]string{domain.DimSystem: sysDim}),
			tagCol: `x.tag_set_id`, team: `x.team_id`, agent: `x.agent_id`, pool: `x.pool_id`, system: `x.system_id`,
			measures: []string{bigint(`coalesce(sum(x.connections) FILTER (WHERE x.verdict = 'allowed'), 0)`),
				bigint(`coalesce(sum(x.connections) FILTER (WHERE x.verdict = 'denied'), 0)`),
				`sum(x.requests) FILTER (WHERE x.verdict = 'allowed')::bigint`, `sum(x.errors) FILTER (WHERE x.verdict = 'allowed')::bigint`,
				bigint(`coalesce(sum(x.bytes_out), 0)`), bigint(`coalesce(sum(x.bytes_in), 0)`)}}, true
	}
	return reportSource{}, false
}

// rollupDims are the dimensions of the daily rollup tables (no hour: a range over two days cannot group by it).
func rollupDims() map[domain.ReportDim]string {
	return map[domain.ReportDim]string{
		domain.DimTeam: `x.team_id`, domain.DimAgent: fmt.Sprintf(`coalesce(x.agent_id, %s)`, none),
		domain.DimPool: fmt.Sprintf(`coalesce(x.pool_id, %s)`, none), domain.DimDay: fmt.Sprintf(dayText, `x.day`),
	}
}

// rollupActivity reads afs.agent_activity_rollup_daily (-db 000018), summed to the level the report shows.
// Utilization is 100 * working / (working + idle) over those sums, the figure the 5-minute path gives.
// The measure "workers" of the table is not a report column; across tag groups it would be a maximum.
func rollupActivity() reportSource {
	return reportSource{from: `afs.agent_activity_rollup_daily x`, timeCond: `x.day >= {fromDay}::date AND x.day <= {toDay}::date`,
		dims: rollupDims(), tagCol: `x.tag_set_id`, team: `x.team_id`, agent: `x.agent_id`, pool: `x.pool_id`,
		measures: []string{bigint(`coalesce(sum(x.working_seconds), 0)`), bigint(`coalesce(sum(x.idle_seconds), 0)`),
			bigint(`coalesce(sum(x.off_seconds), 0)`), bigint(`coalesce(sum(x.impaired_seconds), 0)`),
			bigint(`coalesce(sum(x.model_calls), 0)`),
			`round(100.0 * sum(x.working_seconds) / nullif(sum(x.working_seconds) + sum(x.idle_seconds), 0), 1)::float8`}}
}

// rollupTraffic reads afs.external_traffic_rollup_daily. Requests and errors stay NULL where no egress
// proxy saw HTTP (the sum of NULLs is NULL).
func rollupTraffic() reportSource {
	d := rollupDims()
	d[domain.DimSystem] = fmt.Sprintf(`coalesce(x.system_id, %s)`, none)
	return reportSource{from: `afs.external_traffic_rollup_daily x`, timeCond: `x.day >= {fromDay}::date AND x.day <= {toDay}::date`,
		dims: d, tagCol: `x.tag_set_id`, team: `x.team_id`, agent: `x.agent_id`, pool: `x.pool_id`, system: `x.system_id`,
		measures: []string{bigint(`coalesce(sum(x.connections), 0)`), bigint(`coalesce(sum(x.denied_connections), 0)`),
			`sum(x.requests)::bigint`, `sum(x.errors)::bigint`,
			bigint(`coalesce(sum(x.bytes_out), 0)`), bigint(`coalesce(sum(x.bytes_in), 0)`)}}
}

// builder collects statement arguments.
type builder struct{ args []any }

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

// reportSQL returns the grouped statement and the totals statement with their arguments.
func reportSQL(q domain.ReportQuery, offset, limit int) (rowsSQL string, rowsArgs []any, totalsSQL string, totalsArgs []any, err error) {
	src, ok := sourceFor(q)
	if !ok {
		return "", nil, "", nil, domain.NotFound("report")
	}
	b := &builder{}
	cond := src.timeCond
	for _, ph := range []struct {
		name string
		v    any
	}{{"{from}", q.From}, {"{to}", q.To}, {"{fromDay}", q.FirstDay()}, {"{toDay}", q.LastDay()}} {
		if strings.Contains(cond, ph.name) {
			cond = strings.ReplaceAll(cond, ph.name, b.arg(ph.v))
		}
	}
	conds := []string{cond}
	eq := func(col, v string) {
		if v != "" && col != "" {
			conds = append(conds, col+" = "+b.arg(v))
		}
	}
	f := q.Filters
	eq(src.team, f.TeamID)
	eq(src.agent, f.AgentID)
	eq(src.pool, f.PoolID)
	eq(src.model, f.ModelID)
	eq(src.system, f.SystemID)
	join := ""
	if src.tagCol != "" {
		join = ` LEFT JOIN afs.tag_sets ts ON ts.id = ` + src.tagCol
		for _, k := range sortedKeys(f.Tags) {
			if f.Tags[k] == domain.UntaggedValue {
				conds = append(conds, `(ts.tags ->> `+b.arg(k)+`) IS NULL`)
			} else {
				conds = append(conds, `(ts.tags ->> `+b.arg(k)+`) = `+b.arg(f.Tags[k]))
			}
		}
	}
	// Placeholders so far are shared by both statements.
	shared := len(b.args)
	var dimExprs []string
	for _, g := range q.GroupBy {
		if g.Dim == domain.DimTag {
			dimExprs = append(dimExprs, fmt.Sprintf(`coalesce(ts.tags ->> %s, '%s')`, b.arg(g.Key), domain.UntaggedValue))
			continue
		}
		e, ok := src.dims[g.Dim]
		if !ok {
			return "", nil, "", nil, domain.Validation(domain.FieldError{Field: "groupBy", Reason: fmt.Sprintf("%s reports cannot be grouped by %s", q.Kind, g.Dim)})
		}
		dimExprs = append(dimExprs, e)
	}
	where := ` WHERE ` + strings.Join(conds, ` AND `)
	pos := make([]string, len(dimExprs))
	for i := range dimExprs {
		pos[i] = fmt.Sprint(i + 1)
	}
	rowsSQL = `SELECT ` + strings.Join(dimExprs, `, `) + `, ` + strings.Join(src.measures, `, `) + ` FROM ` + src.from + join + where +
		` GROUP BY ` + strings.Join(pos, `, `) + ` ORDER BY ` + strings.Join(pos, `, `) +
		` OFFSET ` + b.arg(offset) + ` LIMIT ` + b.arg(limit)
	rowsArgs = b.args
	totalsSQL = `SELECT ` + strings.Join(src.measures, `, `) + ` FROM ` + src.from + join + where
	totalsArgs = rowsArgs[:shared]
	return rowsSQL, rowsArgs, totalsSQL, totalsArgs, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// scanMeasures reads the measure columns of a row into JSON-ready values.
type measureScanner struct {
	types []domain.Measure
	ints  []*int64
	flts  []*float64
	strs  []*string
}

func newMeasureScanner(ms []domain.Measure) *measureScanner {
	s := &measureScanner{types: ms, ints: make([]*int64, len(ms)), flts: make([]*float64, len(ms)), strs: make([]*string, len(ms))}
	return s
}

func (s *measureScanner) targets() []any {
	out := make([]any, len(s.types))
	for i, m := range s.types {
		switch m.Type {
		case domain.MeasureInt:
			out[i] = &s.ints[i]
		case domain.MeasureFloat:
			out[i] = &s.flts[i]
		case domain.MeasureMoney:
			out[i] = &s.strs[i]
		}
	}
	return out
}

func (s *measureScanner) values() ([]any, error) {
	out := make([]any, len(s.types))
	for i, m := range s.types {
		switch m.Type {
		case domain.MeasureInt:
			if s.ints[i] != nil {
				out[i] = *s.ints[i]
			}
		case domain.MeasureFloat:
			if s.flts[i] != nil {
				out[i] = *s.flts[i]
			}
		case domain.MeasureMoney:
			if s.strs[i] != nil {
				mny, err := domain.ParseMoney(m.Name, *s.strs[i])
				if err != nil {
					return nil, err
				}
				out[i] = mny.String()
			}
		}
	}
	s.ints, s.flts, s.strs = make([]*int64, len(s.types)), make([]*float64, len(s.types)), make([]*string, len(s.types))
	return out, nil
}

// Run implements reports.Repository.
func (*ReportsRepo) Run(ctx context.Context, tx port.Tx, q domain.ReportQuery, offset, limit int) (reports.Result, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return reports.Result{}, err
	}
	rowsSQL, rowsArgs, totalsSQL, totalsArgs, err := reportSQL(q, offset, limit)
	if err != nil {
		return reports.Result{}, err
	}
	measures := domain.ReportMeasures(q.Kind)
	sc := newMeasureScanner(measures)
	var res reports.Result
	if err := func() error {
		rows, err := t.Query(ctx, rowsSQL, rowsArgs...)
		if err != nil {
			return MapError(err, OpRead, "report")
		}
		defer rows.Close()
		for rows.Next() {
			group := make([]string, len(q.GroupBy))
			dest := make([]any, 0, len(group)+len(measures))
			for i := range group {
				dest = append(dest, &group[i])
			}
			dest = append(dest, sc.targets()...)
			if err := rows.Scan(dest...); err != nil {
				return MapError(err, OpRead, "report")
			}
			vals, err := sc.values()
			if err != nil {
				return err
			}
			res.Rows = append(res.Rows, domain.ReportRow{Group: group, Measures: vals})
		}
		return MapError(rows.Err(), OpRead, "report")
	}(); err != nil {
		return reports.Result{}, err
	}
	if err := t.QueryRow(ctx, totalsSQL, totalsArgs...).Scan(sc.targets()...); err != nil {
		return reports.Result{}, MapError(err, OpRead, "report totals")
	}
	if res.Totals, err = sc.values(); err != nil {
		return reports.Result{}, err
	}
	return res, nil
}
