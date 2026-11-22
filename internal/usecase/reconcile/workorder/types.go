package workorder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Work order statuses (WO-5).
const (
	StatusOpen       = "open"
	StatusAssigned   = "assigned"
	StatusInProgress = "in_progress"
	StatusPROpened   = "pr_opened"
	StatusDone       = "done"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
)

// Repository statuses (WO-3).
const (
	RepoPending     = "pending"
	RepoInProgress  = "in_progress"
	RepoPROpened    = "pr_opened"
	RepoMerged      = "merged"
	RepoNotAffected = "not_affected"
	RepoFailed      = "failed"
	RepoSkipped     = "skipped"
)

// SLA states of the work_order_progress view.
const (
	SLADueSoon  = "due_soon"
	SLABreached = "breached"
)

// Notification kinds (the work_order_notifications check).
const (
	KindAssigned    = "assigned"
	KindPROpened    = "pr_opened"
	KindMerged      = "merged"
	KindSLAWarning  = "sla_warning"
	KindSLABreached = "sla_breached"
	KindCompleted   = "completed"
)

// SentinelRecipient is the recipient of the row that records "the work order's own event of this kind was emitted"
// (sla_warning, sla_breached): the notification table needs a recipient, and the event is the work order's, not
// an owner's.
const SentinelRecipient = "afs"

// Tags of the secrets this package creates (the team tag lets the team delete, RC-17, find them).
const (
	TagTeamID      = "afs-team-id"
	TagPoolID      = "afs-pool-id"
	TagWorkOrderID = "afs-work-order-id"
)

// Clock is the time source.
type Clock func() time.Time

// Scheduler runs a function on an interval while the process leads (engine.Engine.Every).
type Scheduler interface {
	Every(name string, interval time.Duration, fn func(ctx context.Context) error)
}

// IsTerminalRepo reports whether a repository needs no more work for the work order to be done: merged, not
// affected or skipped (WO-5).
func IsTerminalRepo(status string) bool {
	return status == RepoMerged || status == RepoNotAffected || status == RepoSkipped
}

// IsTerminal reports whether the work order is finished.
func IsTerminal(status string) bool {
	return status == StatusDone || status == StatusFailed || status == StatusCancelled
}

// Repo is one repository of a work order, as the reconciler needs it.
type Repo struct {
	Host string
	// Repo is "owner/name".
	Repo          string
	Status        string
	PRURL         string
	PRNumber      int
	OwningTeamID  string
	OwningContact string
}

// Key is "host/owner/name".
func (r Repo) Key() string { return r.Host + "/" + r.Repo }

// Owners returns the recipients of the repository's notifications: the owning team id and the contact address.
func (r Repo) Owners() []string {
	var o []string
	if r.OwningTeamID != "" {
		o = append(o, r.OwningTeamID)
	}
	if r.OwningContact != "" {
		o = append(o, r.OwningContact)
	}
	return o
}

// WorkOrder is a work order the jobs track.
type WorkOrder struct {
	ID           string
	Kind         string
	ExternalRef  string
	Title        string
	Severity     string
	Status       string
	TeamID       string
	TeamTornDown bool
	SLADueAt     time.Time
	// SLAState is on_track, due_soon, breached, met or missed, from the database (work_order_progress).
	SLAState string
	Repos    []Repo
}

// Remaining returns the repositories that are not yet merged, not affected or skipped.
func (w WorkOrder) Remaining() []Repo {
	var out []Repo
	for _, r := range w.Repos {
		if !IsTerminalRepo(r.Status) {
			out = append(out, r)
		}
	}
	return out
}

// PRRef names a pull request.
type PRRef struct {
	Host   string
	Owner  string
	Repo   string
	Number int
}

// String is "host/owner/repo#number".
func (p PRRef) String() string { return fmt.Sprintf("%s/%s/%s#%d", p.Host, p.Owner, p.Repo, p.Number) }

// PRState is what GitHub says about a pull request.
type PRState struct {
	// State is "open" or "closed".
	State    string
	Merged   bool
	MergedAt time.Time
	URL      string
}

// GitHub reads pull request state. A failure that has a time to retry (a rate limit, a 5xx) implements
// Backoff; the caller then keeps the last known state and does not ask again for that installation before then.
type GitHub interface {
	PullRequest(ctx context.Context, ref PRRef) (PRState, error)
}

// ErrNoInstallation means no GitHub App is configured for the host and organization of a repository.
var ErrNoInstallation = errors.New("workorder: no GitHub App installation for the repository's organization")

// Backoff is implemented by a GitHub error that names when to try again.
type Backoff interface {
	error
	// RetryAt is the earliest time to ask again.
	RetryAt() time.Time
	// Scope names what is blocked: the installation (host/org). Repositories of other scopes may still be read.
	Scope() string
}

// ParsePRRef returns the pull request a repository records: host, owner/name and the number from pr_number or the
// pr_url path (.../pull/<n>).
func ParsePRRef(r Repo) (PRRef, bool) {
	owner, name, ok := strings.Cut(r.Repo, "/")
	if !ok || owner == "" || name == "" {
		return PRRef{}, false
	}
	n := r.PRNumber
	if n <= 0 {
		i := strings.LastIndex(r.PRURL, "/pull/")
		if i < 0 {
			return PRRef{}, false
		}
		var v int
		if _, err := fmt.Sscanf(r.PRURL[i+len("/pull/"):], "%d", &v); err != nil || v <= 0 {
			return PRRef{}, false
		}
		n = v
	}
	return PRRef{Host: r.Host, Owner: owner, Repo: name, Number: n}, true
}
