// Package costtags is afs-billing's daily cost allocation tag check (BI-6, M5-F1, TAG-7): for each AFS tag
// key it records whether the Cost and Usage Report has shown the key (and when, first and last) and
// whether AWS Billing has the key active as a cost allocation tag, and raises one cost_tags.not_seen event
// for a key that has been active for 3 days without the CUR showing it. The rows are what ListCostTagStatus
// reads (M5-F2) and the S9 activation banner is built on.
//
// The AFS tag keys are the system keys (afs-team-id, afs-agent-id, afs-pool-id), the service tags and the
// teams' tags. "Seen in the CUR" is read from what cur-ingest already stored: a key is seen on a day when a
// CUR cost_daily row of that day was billed with it (the key is in the row's tag set; the agent and pool
// ids are the evidence for afs-agent-id and afs-pool-id, and storage rows, which only the afs-team-id tag
// can attribute, for afs-team-id). So the check needs no second CUR query and agrees with the costs the
// reports show. The activation status comes from the AWS Billing and Cost Management API
// (ListCostAllocationTags, adapter/aws/costexplorer).
//
// One event per activation: the check derives whether the previous run already found the key overdue from
// that run's own row (its checked_at and AWS status) and the activation time AWS reports now, so a rerun,
// or the next day's run, raises nothing more; deactivating and activating the key again starts a new
// period.
package costtags

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// NotSeenAfter is how long a key may be active before the CUR must have shown it (BI-6).
const NotSeenAfter = 72 * time.Hour

// System tag keys (gateway contract §6).
var SystemKeys = []string{"afs-team-id", "afs-agent-id", "afs-pool-id"}

// AWS statuses as the table stores them.
const (
	StatusActive   = "active"
	StatusInactive = "inactive"
	StatusUnknown  = "unknown"
)

// Activation is what AWS Billing says of a tag key.
type Activation struct {
	// Active is true when the key is an active cost allocation tag.
	Active bool
	// Since is when the status last changed (the activation time of an active key); zero when AWS gave none.
	Since time.Time
}

// Billing reads the user-defined cost allocation tags AWS Billing knows. A key it does not list has no
// status (unknown).
type Billing interface {
	ListTags(ctx context.Context) (map[string]Activation, error)
}

// Seen is when the CUR showed a key.
type Seen struct{ First, Last time.Time }

// Stored is a key's row as the last run left it.
type Stored struct {
	First, Last *time.Time
	AWSStatus   string
	CheckedAt   time.Time
}

// Row is a key's row to write.
type Row struct {
	Key         string
	First, Last *time.Time
	AWSStatus   string
	CheckedAt   time.Time
}

// Store is the billing store; every method runs inside the transaction it is given.
type Store interface {
	// Keys returns the AFS tag keys: system, service and team.
	Keys(ctx context.Context, tx port.Tx) ([]string, error)
	// SeenInCUR returns, for each of keys the CUR rows show, the first and last day.
	SeenInCUR(ctx context.Context, tx port.Tx, keys []string) (map[string]Seen, error)
	// Stored returns every key's row.
	Stored(ctx context.Context, tx port.Tx) (map[string]Stored, error)
	// Save upserts rows.
	Save(ctx context.Context, tx port.Tx, rows []Row) error
}

// Checker runs the daily check.
type Checker struct {
	Billing Billing
	Store   Store
	Tx      port.TxRunner
	Outbox  port.OutboxWriter
	Clock   port.Clock
	IDs     port.IDGen
	Log     *slog.Logger
	// Session is the database session of the job (system:afs-billing).
	Session port.Session
}

// Result is what a run did.
type Result struct {
	Keys int
	// Seen counts keys the CUR has shown; Active counts keys AWS has active.
	Seen, Active int
	// NotSeen lists the keys that raised cost_tags.not_seen.
	NotSeen []string
	// BillingErr is set when AWS Billing could not be read: the statuses are left as they were and the
	// run still records what the CUR shows.
	BillingErr error
}

var keyRE = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,128}$`)

// Run checks every AFS tag key. AWS Billing is read first, outside the transaction; when it fails the
// CUR side is still recorded and the error is returned with the result so the job fails visibly.
func (c *Checker) Run(ctx context.Context) (Result, error) {
	var res Result
	act, billingErr := c.Billing.ListTags(ctx)
	res.BillingErr = billingErr
	now := c.Clock.Now().UTC()
	err := c.Tx.InTx(ctx, c.Session, func(ctx context.Context, tx port.Tx) error {
		res = Result{BillingErr: billingErr}
		keys, err := c.Store.Keys(ctx, tx)
		if err != nil {
			return fmt.Errorf("read tag keys: %w", err)
		}
		keys = validKeys(keys, c.Log)
		seen, err := c.Store.SeenInCUR(ctx, tx, keys)
		if err != nil {
			return fmt.Errorf("read the keys the CUR shows: %w", err)
		}
		stored, err := c.Store.Stored(ctx, tx)
		if err != nil {
			return fmt.Errorf("read the stored status: %w", err)
		}
		res.Keys = len(keys)
		var rows []Row
		for _, k := range keys {
			prev, had := stored[k]
			row := Row{Key: k, First: prev.First, Last: prev.Last, CheckedAt: now}
			if s, ok := seen[k]; ok {
				row.First, row.Last = earliest(row.First, s.First), latest(row.Last, s.Last)
			}
			var a Activation
			var known bool
			switch {
			case billingErr != nil:
				row.AWSStatus = StatusUnknown
				if had {
					row.AWSStatus = prev.AWSStatus // AWS could not be asked: keep what it last said
				}
			default:
				a, known = act[k]
				switch {
				case !known:
					row.AWSStatus = StatusUnknown
				case a.Active:
					row.AWSStatus = StatusActive
				default:
					row.AWSStatus = StatusInactive
				}
			}
			if row.First != nil {
				res.Seen++
			}
			if row.AWSStatus == StatusActive {
				res.Active++
			}
			alreadyRaised := had && overdue(prev.CheckedAt, Row{First: prev.First, AWSStatus: prev.AWSStatus}, a)
			if billingErr == nil && overdue(now, row, a) && !alreadyRaised {
				if err := c.publish(ctx, tx, k, a, now); err != nil {
					return err
				}
				res.NotSeen = append(res.NotSeen, k)
			}
			rows = append(rows, row)
		}
		return c.Store.Save(ctx, tx, rows)
	})
	if err != nil {
		return res, fmt.Errorf("check cost allocation tags: %w", err)
	}
	if c.Log != nil {
		c.Log.Info("cost allocation tags checked", "keys", res.Keys, "seenInCUR", res.Seen, "active", res.Active, "notSeen", res.NotSeen, "billingErr", billingErr)
	}
	if billingErr != nil {
		return res, fmt.Errorf("read cost allocation tag status from AWS Billing: %w", billingErr)
	}
	return res, nil
}

// overdue reports whether, at time at, the key was active for NotSeenAfter and the CUR had not shown it.
// The activation time is the one AWS reports (a is current); a key without one is never overdue.
func overdue(at time.Time, r Row, a Activation) bool {
	return r.AWSStatus == StatusActive && r.First == nil && !a.Since.IsZero() && at.Sub(a.Since) >= NotSeenAfter
}

func (c *Checker) publish(ctx context.Context, tx port.Tx, key string, a Activation, now time.Time) error {
	id, err := c.IDs.NewID("evt")
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(domain.EventCostTagsNotSeen, id.String(), now, c.Session.Actor, "", "cost_tag", key, map[string]any{
		"key": key, "awsStatus": StatusActive, "activeSince": a.Since.UTC().Format(time.RFC3339),
		"daysActive": int(now.Sub(a.Since) / (24 * time.Hour)),
	})
	if err != nil {
		return err
	}
	return c.Outbox.Publish(ctx, tx, ev)
}

func validKeys(keys []string, log *slog.Logger) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		if !keyRE.MatchString(k) {
			if log != nil {
				log.Warn("tag key skipped: not storable in the status table", "len", len(k))
			}
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func earliest(a *time.Time, b time.Time) *time.Time {
	if a == nil || b.Before(*a) {
		return &b
	}
	return a
}

func latest(a *time.Time, b time.Time) *time.Time {
	if a == nil || b.After(*a) {
		return &b
	}
	return a
}
