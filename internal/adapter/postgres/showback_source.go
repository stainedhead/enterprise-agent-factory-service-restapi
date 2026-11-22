package postgres

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/showback"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ShowbackSource implements showback.Source on afs.cost_daily. Calls and tokens are summed from the
// gateway rows (the CUR rows carry cost only, as in the costs report); estimated cost is the gateway
// rows' cost and billed cost the CUR rows'. Money is summed in millionths of a USD so nothing rounds.
type ShowbackSource struct{}

// NewShowbackSource returns a ShowbackSource.
func NewShowbackSource() *ShowbackSource { return &ShowbackSource{} }

var _ showback.Source = (*ShowbackSource)(nil)

const showbackMeasures = `coalesce(sum(c.calls) FILTER (WHERE c.source = 'gateway'), 0)::bigint,
  coalesce(sum(c.input_tokens) FILTER (WHERE c.source = 'gateway'), 0)::bigint,
  coalesce(sum(c.output_tokens) FILTER (WHERE c.source = 'gateway'), 0)::bigint,
  (coalesce(sum(c.cost_usd) FILTER (WHERE c.source = 'gateway'), 0) * 1000000)::bigint,
  (coalesce(sum(c.cost_usd) FILTER (WHERE c.source <> 'gateway'), 0) * 1000000)::bigint`

// Totals implements showback.Source.
func (*ShowbackSource) Totals(ctx context.Context, tx port.Tx, first, last time.Time) (showback.Measures, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return showback.Measures{}, err
	}
	var m showback.Measures
	err = pt.QueryRow(ctx, `SELECT `+showbackMeasures+` FROM afs.cost_daily c WHERE c.day >= $1::date AND c.day <= $2::date`, first, last).
		Scan(&m.Calls, &m.InputTokens, &m.OutputTokens, &m.EstimatedMicros, &m.BilledMicros)
	return m, MapError(err, OpRead, "cost")
}

// ByCostCenter implements showback.Source: the cost center is the team's, including a deleted team's.
func (*ShowbackSource) ByCostCenter(ctx context.Context, tx port.Tx, first, last time.Time) ([]showback.Line, error) {
	return showbackLines(ctx, tx, `SELECT t.cost_center, `+showbackMeasures+`
FROM afs.cost_daily c JOIN afs.teams t ON t.id = c.team_id
WHERE c.day >= $1::date AND c.day <= $2::date GROUP BY t.cost_center ORDER BY t.cost_center`, first, last)
}

// ByTag implements showback.Source: the value of the key in the tag set stamped on the cost row, that
// is, the tags in effect when the cost happened. A row with no tag set, or whose set lacks the key, is
// "(untagged)".
func (*ShowbackSource) ByTag(ctx context.Context, tx port.Tx, first, last time.Time, key string) ([]showback.Line, error) {
	return showbackLines(ctx, tx, `SELECT coalesce(ts.tags ->> $3, '`+domain.UntaggedValue+`'), `+showbackMeasures+`
FROM afs.cost_daily c LEFT JOIN afs.tag_sets ts ON ts.team_id = c.team_id AND ts.id = c.tag_set_id
WHERE c.day >= $1::date AND c.day <= $2::date GROUP BY 1 ORDER BY 1`, first, last, key)
}

func showbackLines(ctx context.Context, tx port.Tx, sql string, args ...any) ([]showback.Line, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, sql, args...)
	if err != nil {
		return nil, MapError(err, OpRead, "cost")
	}
	defer rows.Close()
	out := []showback.Line{}
	for rows.Next() {
		var l showback.Line
		if err := rows.Scan(&l.Value, &l.Calls, &l.InputTokens, &l.OutputTokens, &l.EstimatedMicros, &l.BilledMicros); err != nil {
			return nil, MapError(err, OpRead, "cost")
		}
		out = append(out, l)
	}
	return out, MapError(rows.Err(), OpRead, "cost")
}
