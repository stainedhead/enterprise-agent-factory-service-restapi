// Package cur ingests the AWS Cost and Usage Report into cost_daily (BI-1, BI-7): Bedrock cost by
// inference profile, EKS split cost by namespace (and dedicated nodes by tag), and EBS volumes and
// snapshots by the afs-team-id tag. A day's rows are replaced as a unit, so a rerun is idempotent.
//
// The Fetcher (the Athena adapter) returns aggregated lines already classified by Kind; this package
// attributes each line to a team, agent or pool and model, builds the tag set the line was billed
// with, sums lines that share all of that, and asks the Store to replace the day.
package cur

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Kind classifies a CUR line; the adapter decides it from the line item's product and usage type.
type Kind string

// Line kinds.
const (
	// KindBedrock is Bedrock model usage; Resource is the inference profile ARN.
	KindBedrock Kind = "bedrock"
	// KindPod is the split cost of a pod; Resource is its namespace.
	KindPod Kind = "pod"
	// KindNode is the full cost of a dedicated node, attributed by tag.
	KindNode Kind = "dedicated_node"
	// KindStorage is an EBS volume or snapshot, attributed by tag.
	KindStorage Kind = "storage"
)

// Sources are the cost_daily.source values CUR rows use.
const (
	SourceBedrock = "cur_bedrock"
	SourceEKS     = "cur_eks"
	SourceOther   = "cur_other"
)

// Tag keys the attribution reads (gateway contract §6; the keys are the same on every AWS resource).
const (
	TagTeamID  = "afs-team-id"
	TagAgentID = "afs-agent-id"
	TagPoolID  = "afs-pool-id"
)

// Line is one aggregated CUR row for a day.
type Line struct {
	// Day is the usage day (UTC midnight).
	Day  time.Time
	Kind Kind
	// Resource is the inference profile ARN (Bedrock) or the namespace (pod); empty otherwise.
	Resource string
	// Cost is the unblended cost; for a pod the split cost including its share of unused capacity.
	Cost domain.Money
	// Tags are the line item's resource tags with the CUR key prefix removed (afs-team-id, business-unit).
	Tags map[string]string
}

// Fetcher reads the CUR lines of one UTC day.
type Fetcher interface {
	FetchDay(ctx context.Context, day time.Time) ([]Line, error)
}

// ProfileRef is what a Bedrock inference profile belongs to.
type ProfileRef struct {
	TeamID  string
	ModelID string
}

// Directory is what attribution needs from the database.
type Directory struct {
	// Namespaces maps a team namespace to its team id.
	Namespaces map[string]string
	// Teams is the set of team ids (a deleted team stays until purged).
	Teams map[string]bool
	// Profiles maps an inference profile ARN to its team and model.
	Profiles map[string]ProfileRef
	// TagKeys is the set of managed tag keys (service and team tags); other tags on a line are not
	// part of a tag set.
	TagKeys map[string]bool
	// ProfilesUnavailable is true when the store may not read the inference profiles: Bedrock lines are
	// then attributed by tag only and carry no model.
	ProfilesUnavailable bool
}

// Row is one cost_daily row to write for a day.
type Row struct {
	TeamID  string
	Source  string
	AgentID string
	PoolID  string
	ModelID string
	// Tags are the managed tags the spend was billed with; empty means untagged (tag_set_id NULL).
	Tags map[string]string
	Cost domain.Money
}

// Store reads the directory and replaces a day.
type Store interface {
	Directory(ctx context.Context) (Directory, error)
	// ReplaceDay replaces every CUR row (cur_*) of day with rows in one transaction, building the tag
	// sets from the rows' Tags.
	ReplaceDay(ctx context.Context, day time.Time, rows []Row) error
}

// Attribution is the result of attributing a day's lines.
type Attribution struct {
	Rows []Row
	// Unattributed is the cost of lines no team could be found for.
	Unattributed domain.Money
	// UnattributedLines counts those lines.
	UnattributedLines int
}

// Attribute maps lines to rows. Lines with the same team, source, subject, model and tags are summed,
// so a day with a tag change keeps one row per tag set (BI-7). Rows are sorted for stable output.
func Attribute(lines []Line, dir Directory) Attribution {
	type key struct{ team, source, agent, pool, model, tags string }
	sums := map[key]*Row{}
	var out Attribution
	for _, l := range lines {
		team, model := dir.teamOf(l)
		if team == "" || !dir.Teams[team] {
			out.Unattributed = out.Unattributed.Add(l.Cost)
			out.UnattributedLines++
			continue
		}
		r := Row{TeamID: team, Source: sourceOf(l.Kind), ModelID: model, Tags: dir.managed(l.Tags), Cost: l.Cost}
		if pool := l.Tags[TagPoolID]; pool != "" {
			r.PoolID = pool // a pool worker carries its pool, never an agent
		} else {
			r.AgentID = l.Tags[TagAgentID]
		}
		k := key{r.TeamID, r.Source, r.AgentID, r.PoolID, r.ModelID, tagKey(r.Tags)}
		if cur, ok := sums[k]; ok {
			cur.Cost = cur.Cost.Add(r.Cost)
			continue
		}
		sums[k] = &r
	}
	for _, r := range sums {
		// A day's credits and refunds can net a group to zero or below; cost_daily holds spend only.
		if r.Cost.Micros() > 0 {
			out.Rows = append(out.Rows, *r)
		}
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		ka := strings.Join([]string{a.TeamID, a.Source, a.AgentID, a.PoolID, a.ModelID, tagKey(a.Tags)}, "\x00")
		kb := strings.Join([]string{b.TeamID, b.Source, b.AgentID, b.PoolID, b.ModelID, tagKey(b.Tags)}, "\x00")
		return ka < kb
	})
	return out
}

func sourceOf(k Kind) string {
	switch k {
	case KindBedrock:
		return SourceBedrock
	case KindPod, KindNode:
		return SourceEKS
	}
	return SourceOther
}

// teamOf finds a line's team (and, for Bedrock, its model). The tag wins; the inference profile and
// the namespace are the fallbacks for lines AWS billed without the tag.
func (d Directory) teamOf(l Line) (team, model string) {
	if l.Kind == KindBedrock {
		if p, ok := d.Profiles[l.Resource]; ok {
			model = p.ModelID
			team = p.TeamID
		}
	}
	if t := l.Tags[TagTeamID]; t != "" {
		team = t
	}
	if team == "" && l.Kind == KindPod {
		team = d.Namespaces[l.Resource]
	}
	return team, model
}

// managed keeps the tags that are part of a tag set: managed keys with a value. System tags (afs-*)
// are not part of a tag set.
func (d Directory) managed(tags map[string]string) map[string]string {
	var out map[string]string
	for k, v := range tags {
		if v == "" || !d.TagKeys[k] || strings.HasPrefix(k, "afs-") || strings.HasPrefix(k, "aws:") {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

func tagKey(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + tags[k] + "\x00")
	}
	return b.String()
}

// Backoff is the retry schedule of an Athena failure.
type Backoff struct {
	// Attempts is the number of tries per day (default 4: the first and three retries).
	Attempts int
	// Base is the wait before the first retry; it doubles each time up to Max.
	Base, Max time.Duration
}

// DefaultBackoff retries three times: 5 s, 10 s, 20 s.
var DefaultBackoff = Backoff{Attempts: 4, Base: 5 * time.Second, Max: time.Minute}

func (b Backoff) wait(retry int) time.Duration {
	d := b.Base << (retry - 1)
	if d > b.Max || d <= 0 {
		d = b.Max
	}
	return d
}

// AlertAfter is the number of failed days in one run that raises the alert (BI-1: "alerts after 3
// failed days").
const AlertAfter = 3

// Alert says the ingest keeps failing.
type Alert struct {
	// FailedDays are the days whose ingest failed after every retry, oldest first.
	FailedDays []time.Time
	// Err is the last failure.
	Err error
}

// Ingestor runs the ingest.
type Ingestor struct {
	Fetch Fetcher
	Store Store
	Log   *slog.Logger
	Retry Backoff
	// Sleep waits d or returns the context's error; nil uses a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnAlert is called once per run when AlertAfter days failed; nil only logs.
	OnAlert func(Alert)
}

// DayResult is what one day's ingest did.
type DayResult struct {
	Day  time.Time
	Rows int
	Cost domain.Money
	// Unattributed is the cost no team could be found for.
	Unattributed domain.Money
	// NoData is true when the CUR had no lines yet, so the day's rows were left as they were.
	NoData bool
	Err    error
}

// Result is what a run did.
type Result struct {
	Days   []DayResult
	Failed int
}

// Err is non-nil when any day failed.
func (r Result) Err() error {
	var errs []error
	for _, d := range r.Days {
		if d.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Day.Format(time.DateOnly), d.Err))
		}
	}
	return errors.Join(errs...)
}

func (i *Ingestor) sleep(ctx context.Context, d time.Duration) error {
	if i.Sleep != nil {
		return i.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (i *Ingestor) log() *slog.Logger {
	if i.Log != nil {
		return i.Log
	}
	return slog.Default()
}

// Run ingests days, oldest first. A day that fails after its retries does not stop the others; the
// run alerts when AlertAfter or more days failed.
func (i *Ingestor) Run(ctx context.Context, days []time.Time) Result {
	var res Result
	var failed []time.Time
	var last error
	for _, day := range days {
		dr := i.runDay(ctx, day)
		res.Days = append(res.Days, dr)
		if dr.Err != nil {
			res.Failed++
			failed = append(failed, day)
			last = dr.Err
			if ctx.Err() != nil {
				break
			}
		}
	}
	if len(failed) >= AlertAfter {
		i.log().Error("CUR ingest failed for several days", "alert", true, "failedDays", len(failed), "err", last)
		if i.OnAlert != nil {
			i.OnAlert(Alert{FailedDays: failed, Err: last})
		}
	}
	return res
}

func (i *Ingestor) runDay(ctx context.Context, day time.Time) DayResult {
	day = day.UTC().Truncate(24 * time.Hour)
	dr := DayResult{Day: day}
	attempts := i.Retry.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var lines []Line
	var err error
	for n := 1; n <= attempts; n++ {
		if lines, err = i.Fetch.FetchDay(ctx, day); err == nil {
			break
		}
		i.log().Warn("CUR query failed", "day", day.Format(time.DateOnly), "attempt", n, "err", err)
		if n == attempts || ctx.Err() != nil {
			break
		}
		if serr := i.sleep(ctx, i.Retry.wait(n)); serr != nil {
			err = serr
			break
		}
	}
	if err != nil {
		dr.Err = fmt.Errorf("fetch CUR: %w", err)
		return dr
	}
	if len(lines) == 0 {
		dr.NoData = true
		i.log().Warn("no CUR lines for the day yet; rows left unchanged", "day", day.Format(time.DateOnly))
		return dr
	}
	dir, err := i.Store.Directory(ctx)
	if err != nil {
		dr.Err = fmt.Errorf("read directory: %w", err)
		return dr
	}
	if dir.ProfilesUnavailable {
		i.log().Warn("inference profiles are not readable by the billing role; Bedrock cost is attributed by tag and has no model")
	}
	att := Attribute(lines, dir)
	if att.UnattributedLines > 0 {
		i.log().Warn("CUR lines not attributed to a team", "day", day.Format(time.DateOnly),
			"lines", att.UnattributedLines, "costUsd", att.Unattributed.String())
	}
	if err := i.Store.ReplaceDay(ctx, day, att.Rows); err != nil {
		dr.Err = fmt.Errorf("replace day: %w", err)
		return dr
	}
	dr.Rows = len(att.Rows)
	dr.Unattributed = att.Unattributed
	for _, r := range att.Rows {
		dr.Cost = dr.Cost.Add(r.Cost)
	}
	return dr
}

// Days returns the UTC days a run covers: the lookback days ending yesterday, oldest first. CUR is
// restated for days after the first delivery, so each run re-reads a few days.
func Days(now time.Time, lookback int) []time.Time {
	if lookback < 1 {
		lookback = 1
	}
	today := now.UTC().Truncate(24 * time.Hour)
	out := make([]time.Time, 0, lookback)
	for n := lookback; n >= 1; n-- {
		out = append(out, today.AddDate(0, 0, -n))
	}
	return out
}
