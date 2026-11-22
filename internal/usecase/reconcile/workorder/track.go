package workorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Intervals of the tracking jobs. GitHub is read every 10 minutes (RC-12); the work order's own status follows
// the stored repository statuses (which agents report through the API) every half minute.
const (
	PollInterval   = 10 * time.Minute
	SettleInterval = 30 * time.Second
)

// Scope names an installation's blocking scope: "host/org", lower case.
func Scope(host, owner string) string { return strings.ToLower(host + "/" + owner) }

// RepoChange moves one repository after GitHub was read.
type RepoChange struct {
	WorkOrderID string
	TeamID      string
	Host        string
	Repo        string
	// FromStatus and FromPRURL are what the reconciler saw; the change applies only if the row still holds them,
	// so a report an agent made meanwhile is never overwritten.
	FromStatus string
	FromPRURL  string
	// To is merged or in_progress.
	To       string
	PRURL    string
	PRNumber int
	MergedAt time.Time
	Detail   string
}

// StatusChange moves a work order.
type StatusChange struct {
	WorkOrderID string
	TeamID      string
	From        string
	To          string
	At          time.Time
	Reason      string
}

// TrackStore is the database side of tracking.
type TrackStore interface {
	// Active lists the work orders that are not finished, plus those done in the last few days (their
	// completion may still need its notifications), each with its repositories, its team's teardown state and its
	// SLA state.
	Active(ctx context.Context) ([]WorkOrder, error)
	// ApplyRepo applies a repository change if the row still holds FromStatus and FromPRURL, and writes one audit
	// record and the work_order.repo_updated event (and work_order.merged for a merge), in one transaction. It
	// reports false, writing nothing, when the row changed meanwhile.
	ApplyRepo(ctx context.Context, c RepoChange) (bool, error)
	// ApplyStatus applies a status change if the work order still has status From, sets completed_at for done and
	// failed, and writes one audit record and, for done and failed, the work_order.completed or work_order.failed
	// event, in one transaction. It reports false, writing nothing, when the status changed meanwhile.
	ApplyStatus(ctx context.Context, c StatusChange) (bool, error)
}

// DeriveStatus is the work order's status given its repositories (WO-5):
//
//   - done when every repository is merged, not affected or skipped;
//   - failed when its team has been torn down while repositories remain;
//   - pr_opened when every remaining repository has a pull request open;
//   - in_progress when a repository has started;
//   - assigned otherwise.
//
// Open (no team yet), cancelled, done and failed work orders keep their status; so does a work order without
// repositories.
func DeriveStatus(w WorkOrder) string {
	if w.Status == StatusOpen || IsTerminal(w.Status) || len(w.Repos) == 0 {
		return w.Status
	}
	rem := w.Remaining()
	if len(rem) == 0 {
		return StatusDone
	}
	if w.TeamTornDown {
		return StatusFailed
	}
	allPR := true
	for _, r := range rem {
		if r.Status != RepoPROpened {
			allPR = false
		}
	}
	if allPR {
		return StatusPROpened
	}
	for _, r := range w.Repos {
		if r.Status != RepoPending {
			return StatusInProgress
		}
	}
	return StatusAssigned
}

// Tracker follows the pull requests of work orders and settles their status.
type Tracker struct {
	Store  TrackStore
	GitHub GitHub // nil: pull requests are not read, agents' reports alone move repositories
	Now    Clock
	Log    *slog.Logger

	blocked map[string]time.Time
	warned  map[string]bool
}

// Poll reads the state of every recorded pull request of an active work order from GitHub and moves the
// repositories: merged for a merged pull request, back to in_progress for one closed without merging (the agent
// must open another). An installation that answers with a rate limit or a server error is left alone until its
// retry time, and the stored state stays as it was. Poll then settles the status of the work orders it changed.
func (t *Tracker) Poll(ctx context.Context) error {
	if t.GitHub == nil {
		return nil
	}
	if t.blocked == nil {
		t.blocked = map[string]time.Time{}
		t.warned = map[string]bool{}
	}
	wos, err := t.Store.Active(ctx)
	if err != nil {
		return fmt.Errorf("workorder poll: list: %w", err)
	}
	var errs []error
	for _, w := range wos {
		if w.Status == StatusOpen || IsTerminal(w.Status) {
			continue
		}
		changed := false
		for i := range w.Repos {
			r := &w.Repos[i]
			if r.Status != RepoPROpened {
				continue
			}
			ok, err := t.pollRepo(ctx, w, r)
			if err != nil {
				errs = append(errs, err)
			}
			changed = changed || ok
		}
		if changed {
			if err := t.settle(ctx, w); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// pollRepo reads one pull request. It reports whether the repository changed (r is updated).
func (t *Tracker) pollRepo(ctx context.Context, w WorkOrder, r *Repo) (bool, error) {
	ref, ok := ParsePRRef(*r)
	if !ok {
		t.warnOnce(w.ID+"/"+r.Key(), "a repository records a pull request GitHub cannot be asked about", "workOrder", w.ID, "repo", r.Key(), "prUrl", r.PRURL)
		return false, nil
	}
	scope := Scope(ref.Host, ref.Owner)
	if until, blocked := t.blocked[scope]; blocked {
		if t.Now().Before(until) {
			return false, nil
		}
		delete(t.blocked, scope)
	}
	st, err := t.GitHub.PullRequest(ctx, ref)
	if err != nil {
		var b Backoff
		switch {
		case errors.As(err, &b):
			t.blocked[b.Scope()] = b.RetryAt()
			t.warnOnce(b.Scope()+"/"+b.RetryAt().Format(time.RFC3339), "GitHub is backing off; the last known pull request state is kept", "scope", b.Scope(), "retryAt", b.RetryAt(), "reason", err.Error())
			return false, nil
		case errors.Is(err, ErrNoInstallation):
			t.warnOnce(scope, "no GitHub App is configured for this organization; its pull requests are not read", "scope", scope)
			return false, nil
		}
		return false, fmt.Errorf("workorder poll: %s: %w", ref, err)
	}
	c := RepoChange{WorkOrderID: w.ID, TeamID: w.TeamID, Host: r.Host, Repo: r.Repo, FromStatus: r.Status, FromPRURL: r.PRURL, PRURL: r.PRURL, PRNumber: ref.Number}
	switch {
	case st.Merged:
		c.To = RepoMerged
		c.MergedAt = st.MergedAt
		if c.MergedAt.IsZero() {
			c.MergedAt = t.Now()
		}
	case st.State == "closed":
		c.To = RepoInProgress
		c.Detail = "the pull request was closed without merging"
	default:
		return false, nil
	}
	applied, err := t.Store.ApplyRepo(ctx, c)
	if err != nil {
		return false, fmt.Errorf("workorder poll: %s: %w", ref, err)
	}
	if applied {
		r.Status = c.To
		t.log().Info("repository moved from the pull request's state", "workOrder", w.ID, "repo", r.Key(), "status", c.To)
	}
	return applied, nil
}

// Settle moves every active work order to the status its repositories give (DeriveStatus). A done work order
// makes its team due for teardown (rule at_work_complete).
func (t *Tracker) Settle(ctx context.Context) error {
	wos, err := t.Store.Active(ctx)
	if err != nil {
		return fmt.Errorf("workorder settle: list: %w", err)
	}
	var errs []error
	for _, w := range wos {
		if err := t.settle(ctx, w); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t *Tracker) settle(ctx context.Context, w WorkOrder) error {
	to := DeriveStatus(w)
	if to == w.Status {
		return nil
	}
	reason := ""
	if to == StatusFailed {
		reason = fmt.Sprintf("the team was torn down with %d repositories not merged, not affected or skipped", len(w.Remaining()))
	}
	applied, err := t.Store.ApplyStatus(ctx, StatusChange{WorkOrderID: w.ID, TeamID: w.TeamID, From: w.Status, To: to, At: t.Now(), Reason: reason})
	if err != nil {
		return fmt.Errorf("workorder settle: %s: %w", w.ID, err)
	}
	if applied {
		t.log().Info("work order status changed", "workOrder", w.ID, "from", w.Status, "to", to)
	}
	return nil
}

func (t *Tracker) warnOnce(key, msg string, args ...any) {
	if t.warned == nil {
		t.warned = map[string]bool{}
	}
	if t.warned[key] {
		return
	}
	t.warned[key] = true
	t.log().Warn(msg, args...)
}

func (t *Tracker) log() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}
