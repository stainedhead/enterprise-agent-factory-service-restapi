package workorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// NotifyInterval is how often owners are checked for new events.
const NotifyInterval = 30 * time.Second

// NotificationKey identifies one sent notification (the work_order_notifications uniqueness): host and repo are
// empty for the work order's own events.
type NotificationKey struct {
	Host      string
	Repo      string
	Kind      string
	Recipient string
}

// Notification is one notification to one recipient.
type Notification struct {
	NotificationKey
	WorkOrderID string
	// WorkOrderTeamID is the remediation team (empty before assignment); the work order's own events carry it.
	WorkOrderTeamID string
	// ChannelID is the service channel an outside contact is emailed through.
	ChannelID string
	Subject   string
	Body      string
	// Data is the event's data.
	Data map[string]any
}

// IsTeam reports whether the recipient is an AFS team (else an outside contact's address, or the sentinel).
func (k NotificationKey) IsTeam() bool { return strings.HasPrefix(k.Recipient, "team_") }

// NotifyStore is the database side of owner notifications.
type NotifyStore interface {
	// Sent lists the notifications already recorded for the work orders.
	Sent(ctx context.Context, workOrderIDs []string) (map[string]map[NotificationKey]bool, error)
	// NotificationChannel returns the id of the service-level email channel named in work_orders.notification_channel_id,
	// or "" when the setting is unset or names no enabled email channel.
	NotificationChannel(ctx context.Context) (string, error)
	// RecordTeam records the notification and writes the work_order.<kind> event with the recipient team's id as
	// its team (RC-15 delivers it on that team's channels), in one transaction. It reports false when the
	// notification was already recorded.
	RecordTeam(ctx context.Context, n Notification) (bool, error)
	// RecordContact records an email sent to an outside contact. It reports false when already recorded.
	RecordContact(ctx context.Context, n Notification) (bool, error)
	// RecordEvent records the work order's own event of that kind (sla_warning, sla_breached), under the
	// SentinelRecipient, and writes the event with the remediation team's id (none before assignment), in one
	// transaction. It reports false when already recorded.
	RecordEvent(ctx context.Context, n Notification) (bool, error)
}

// ContactMailer sends one email to an outside contact through a service channel.
type ContactMailer interface {
	Send(ctx context.Context, channelID, to, subject, body string) error
}

// Notifier tells the owners of work orders once per event and recipient (RC-US-10.5).
//
// Delivery is at least once toward contacts: an email is sent and then recorded, so a crash between the two sends
// it again on the next pass, while a failed send records nothing and is retried. Toward teams it is exactly once:
// the record and the outbox event share a transaction.
type Notifier struct {
	// Source lists the work orders to consider (TrackStore.Active).
	Source interface {
		Active(ctx context.Context) ([]WorkOrder, error)
	}
	Store  NotifyStore
	Mailer ContactMailer // nil: contacts are not emailed
	Log    *slog.Logger

	warned map[string]bool
}

// Run records and delivers every notification that is due and not yet recorded for the active work orders. A
// failure for one recipient does not stop the others.
func (n *Notifier) Run(ctx context.Context) error {
	wos, err := n.Source.Active(ctx)
	if err != nil {
		return fmt.Errorf("workorder notify: list: %w", err)
	}
	return n.Notify(ctx, wos)
}

// Notify handles the given work orders.
func (n *Notifier) Notify(ctx context.Context, wos []WorkOrder) error {
	if len(wos) == 0 {
		return nil
	}
	ids := make([]string, 0, len(wos))
	for _, w := range wos {
		ids = append(ids, w.ID)
	}
	sent, err := n.Store.Sent(ctx, ids)
	if err != nil {
		return fmt.Errorf("workorder notify: sent: %w", err)
	}
	channel, err := n.Store.NotificationChannel(ctx)
	if err != nil {
		return fmt.Errorf("workorder notify: channel: %w", err)
	}
	var errs []error
	for _, w := range wos {
		for _, c := range Candidates(w) {
			if sent[w.ID][c.NotificationKey] {
				continue
			}
			if err := n.deliver(ctx, c, channel); err != nil {
				errs = append(errs, fmt.Errorf("workorder notify: %s %s to %s: %w", w.ID, c.Kind, redactRecipient(c.Recipient), err))
			}
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) deliver(ctx context.Context, c Notification, channel string) error {
	switch {
	case c.Recipient == SentinelRecipient:
		_, err := n.Store.RecordEvent(ctx, c)
		return err
	case c.IsTeam():
		_, err := n.Store.RecordTeam(ctx, c)
		return err
	}
	// An outside contact: emailed through the service channel; nothing when none is set.
	if channel == "" || n.Mailer == nil {
		n.warnOnce("no-channel", "outside contacts are not emailed: work_orders.notification_channel_id names no enabled email channel")
		return nil
	}
	c.ChannelID = channel
	if err := n.Mailer.Send(ctx, channel, c.Recipient, c.Subject, c.Body); err != nil {
		return err
	}
	_, err := n.Store.RecordContact(ctx, c)
	return err
}

// redactRecipient keeps an address out of error text and logs beyond its domain.
func redactRecipient(r string) string {
	if i := strings.LastIndex(r, "@"); i > 0 {
		return "*" + r[i:]
	}
	return r
}

func (n *Notifier) warnOnce(key, msg string) {
	if n.warned == nil {
		n.warned = map[string]bool{}
	}
	if n.warned[key] {
		return
	}
	n.warned[key] = true
	if n.Log != nil {
		n.Log.Warn(msg)
		return
	}
	slog.Default().Warn(msg)
}

// owners returns the distinct owners of the repositories, sorted.
func owners(repos []Repo) []string {
	set := map[string]bool{}
	for _, r := range repos {
		for _, o := range r.Owners() {
			set[o] = true
		}
	}
	out := make([]string, 0, len(set))
	for o := range set {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// Candidates lists the notifications the work order's current state calls for, in a stable order. Each is
// derived from state, so the list is the same on every pass and the recorded ones are filtered by the caller.
//
//   - assigned: the team exists; every owner, once for the work order.
//   - pr_opened: each repository whose pull request is open or merged; its owners.
//   - merged: each merged repository; its owners.
//   - sla_warning, sla_breached: the SLA state is due_soon or breached and the work order is unfinished; the owners
//     of the repositories not yet done, and the work order's own event.
//   - completed: the work order is done; every owner.
func Candidates(w WorkOrder) []Notification {
	if w.Status == StatusCancelled {
		return nil
	}
	var out []Notification
	add := func(kind string, r *Repo, recipient string, extra map[string]any) {
		n := Notification{WorkOrderID: w.ID, WorkOrderTeamID: w.TeamID}
		n.Kind, n.Recipient = kind, recipient
		n.Data = map[string]any{"workOrderId": w.ID, "kind": kind, "recipient": recipient, "title": w.Title, "severity": w.Severity}
		if w.ExternalRef != "" {
			n.Data["cveId"] = w.ExternalRef
		}
		if w.TeamID != "" {
			n.Data["teamId"] = w.TeamID
		}
		what := ""
		if r != nil {
			n.Host, n.Repo = r.Host, r.Repo
			n.Data["host"], n.Data["repo"] = r.Host, r.Repo
			if r.PRURL != "" {
				n.Data["prUrl"] = r.PRURL
			}
			what = " " + r.Key()
		}
		for k, v := range extra {
			n.Data[k] = v
		}
		n.Subject, n.Body = text(w, kind, what, r)
		out = append(out, n)
	}
	if w.TeamID != "" && w.Status != StatusOpen {
		for _, o := range owners(w.Repos) {
			add(KindAssigned, nil, o, nil)
		}
	}
	for i := range w.Repos {
		r := &w.Repos[i]
		if r.Status == RepoPROpened || r.Status == RepoMerged {
			for _, o := range r.Owners() {
				add(KindPROpened, r, o, nil)
			}
		}
		if r.Status == RepoMerged {
			for _, o := range r.Owners() {
				add(KindMerged, r, o, nil)
			}
		}
	}
	if !IsTerminal(w.Status) {
		kind := ""
		switch w.SLAState {
		case SLADueSoon:
			kind = KindSLAWarning
		case SLABreached:
			kind = KindSLABreached
		}
		if kind != "" {
			extra := map[string]any{"slaDueAt": w.SLADueAt.UTC().Format(time.RFC3339), "slaState": w.SLAState}
			for _, o := range owners(w.Remaining()) {
				add(kind, nil, o, extra)
			}
			add(kind, nil, SentinelRecipient, extra)
		}
	}
	if w.Status == StatusDone {
		for _, o := range owners(w.Repos) {
			add(KindCompleted, nil, o, nil)
		}
	}
	return out
}

func text(w WorkOrder, kind, what string, r *Repo) (subject, body string) {
	head := map[string]string{
		KindAssigned:    "Work order assigned",
		KindPROpened:    "Pull request opened",
		KindMerged:      "Pull request merged",
		KindSLAWarning:  "SLA due soon",
		KindSLABreached: "SLA breached",
		KindCompleted:   "Work order completed",
	}[kind]
	subject = fmt.Sprintf("[AFS] %s: %s%s", head, w.Title, what)
	var b strings.Builder
	fmt.Fprintf(&b, "%s.\n\nWork order: %s\nTitle: %s\n", head, w.ID, w.Title)
	if w.ExternalRef != "" {
		fmt.Fprintf(&b, "CVE: %s\n", w.ExternalRef)
	}
	fmt.Fprintf(&b, "Severity: %s\n", w.Severity)
	if r != nil {
		fmt.Fprintf(&b, "Repository: %s\n", r.Key())
		if r.PRURL != "" {
			fmt.Fprintf(&b, "Pull request: %s\n", r.PRURL)
		}
	}
	if !w.SLADueAt.IsZero() {
		fmt.Fprintf(&b, "SLA due: %s\n", w.SLADueAt.UTC().Format(time.RFC3339))
	}
	return subject, b.String()
}
