// Package coststest is an in-memory costs.Repository for use case tests.
package coststest

import (
	"context"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo is an in-memory store.
type Repo struct {
	// Teams are the teams the caller sees.
	Teams map[string]bool
	// Costs maps a team id to its rows (any order; the fake sorts by day then source).
	Costs map[string][]costs.Row
	Recon []costs.Reconciliation
	// Last records the arguments of the last ListCosts call.
	LastOffset, LastLimit int
	LastFrom, LastTo      time.Time
}

var _ costs.Repository = (*Repo)(nil)

// TeamExists implements costs.Repository.
func (r *Repo) TeamExists(_ context.Context, _ port.Tx, teamID string) error {
	if !r.Teams[teamID] {
		return domain.NotFound("team")
	}
	return nil
}

// ListCosts implements costs.Repository.
func (r *Repo) ListCosts(_ context.Context, _ port.Tx, teamID string, from, to time.Time, offset, limit int) ([]costs.Row, error) {
	r.LastOffset, r.LastLimit, r.LastFrom, r.LastTo = offset, limit, from, to
	var in []costs.Row
	for _, row := range r.Costs[teamID] {
		if !row.Day.Before(from) && !row.Day.After(to) {
			in = append(in, row)
		}
	}
	sort.SliceStable(in, func(i, j int) bool {
		if !in[i].Day.Equal(in[j].Day) {
			return in[i].Day.Before(in[j].Day)
		}
		return in[i].Source < in[j].Source
	})
	if offset >= len(in) {
		return nil, nil
	}
	in = in[offset:]
	if len(in) > limit {
		in = in[:limit]
	}
	return in, nil
}

// ListReconciliations implements costs.Repository.
func (r *Repo) ListReconciliations(_ context.Context, _ port.Tx, status string, after *costs.ReconCursor, limit int) ([]costs.Reconciliation, error) {
	all := append([]costs.Reconciliation(nil), r.Recon...)
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Day.Equal(all[j].Day) {
			return all[i].Day.After(all[j].Day)
		}
		return all[i].TeamID > all[j].TeamID
	})
	var out []costs.Reconciliation
	for _, rec := range all {
		if status != "" && rec.Status != status {
			continue
		}
		if after != nil && !(rec.Day.Before(after.Day) || (rec.Day.Equal(after.Day) && rec.TeamID < after.TeamID)) {
			continue
		}
		out = append(out, rec)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
