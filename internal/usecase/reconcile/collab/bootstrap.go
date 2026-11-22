package collab

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// BoardKeyCandidate is a collaboration agent in managed mode: its board key may not be recorded yet, and its
// first admin may not be invited yet.
type BoardKeyCandidate struct {
	TeamID, AgentID string
	Mode            domain.RegistrationMode
	// Recorded is true when the board key is already recorded.
	Recorded bool
	// PodReady is true when Paperclip is up (the admin invite needs it).
	PodReady bool
}

// BoardKeys is the bootstrap's view of the board key (RC-29). In managed mode the pod's entrypoint writes the
// key to Secrets Manager; the reconciler reads only that name, never a value.
type BoardKeys interface {
	// Pending returns the collaboration agents in managed mode.
	Pending(ctx context.Context) ([]BoardKeyCandidate, error)
	// Find returns the ARN of secret <service>/<namespace>/<agent>/paperclip-board-api-key when it exists.
	Find(ctx context.Context, teamID, agentID string) (arn string, found bool, err error)
	// Record calls afs.record_collab_board_key.
	Record(ctx context.Context, teamID, agentID, arn string) error
}

// Admins finds the person who created the collaboration agent (COL-32).
type Admins interface {
	// CreatorEmail returns the email of the person who created the agent, "" when it was not a person.
	CreatorEmail(ctx context.Context, teamID, agentID string) (string, error)
}

// Bootstrap records the board key, invites the first Paperclip admin and announces the end of the bootstrap
// once (collaboration.bootstrap_completed, COL-32).
//
// The invite URL is a credential: it goes only into the one email to the creating team-admin, never into an
// event, an audit record or a log. Paper, Mail and Admins may be nil, which skips the invite.
type Bootstrap struct {
	Keys   BoardKeys
	Events Events
	Paper  Paperclip
	Mail   Mailer
	Admins Admins
	Log    *slog.Logger

	mu   sync.Mutex
	done map[string]bool // team ids whose first admin is settled in this process
}

// Run checks once. Recording is idempotent. The first admin is settled when Paperclip already has an admin
// invite (this run or an earlier one) or the creator is not a person; the event follows the run that recorded
// the key or sent the invite, so a restart does not repeat it.
func (b *Bootstrap) Run(ctx context.Context) error {
	cands, err := b.Keys.Pending(ctx)
	if err != nil {
		return fmt.Errorf("collab: pending board keys: %w", err)
	}
	var failed int
	for _, c := range cands {
		if err := b.one(ctx, c); err != nil {
			failed++
			if b.Log != nil {
				b.Log.Warn("collaboration bootstrap failed", "team", c.TeamID, "err", err)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: %d bootstraps failed", failed)
	}
	return nil
}

func (b *Bootstrap) settled(team string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done[team]
}

func (b *Bootstrap) settle(team string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done == nil {
		b.done = map[string]bool{}
	}
	b.done[team] = true
}

func (b *Bootstrap) one(ctx context.Context, c BoardKeyCandidate) error {
	announce := false
	if !c.Recorded {
		arn, found, err := b.Keys.Find(ctx, c.TeamID, c.AgentID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := b.Keys.Record(ctx, c.TeamID, c.AgentID, arn); err != nil {
			return err
		}
		announce = true
	}
	if b.Paper != nil && b.Mail != nil && b.Admins != nil && !b.settled(c.TeamID) {
		if !c.PodReady {
			return nil // the event waits for the invite
		}
		invited, err := b.inviteFirstAdmin(ctx, c)
		if err != nil {
			return err
		}
		b.settle(c.TeamID)
		announce = announce || invited
	} else if b.Paper == nil {
		b.settle(c.TeamID)
	}
	if !announce {
		return nil
	}
	// The event carries no invite URL and no key: the invite is a credential (G8).
	return b.Events.Publish(ctx, domain.EventCollaborationBootstrapCompleted, c.TeamID, string(KindTeamCollaboration), c.TeamID, c.AgentID,
		map[string]any{"teamId": c.TeamID, "agentId": c.AgentID, "mode": string(c.Mode)})
}

// inviteFirstAdmin invites the creating team-admin as Paperclip admin unless an admin invite exists. It
// reports whether it sent one.
func (b *Bootstrap) inviteFirstAdmin(ctx context.Context, c BoardKeyCandidate) (bool, error) {
	invites, err := b.Paper.Invites(ctx, c.TeamID)
	if err != nil {
		return false, err
	}
	for _, i := range invites {
		if i.Role == RoleAdmin {
			return false, nil
		}
	}
	email, err := b.Admins.CreatorEmail(ctx, c.TeamID, c.AgentID)
	if err != nil {
		return false, err
	}
	if email == "" {
		if b.Log != nil {
			b.Log.Warn("the collaboration agent was not created by a person: no first admin invite is sent", "team", c.TeamID)
		}
		return false, nil
	}
	inv, err := b.Paper.CreateInvite(ctx, c.TeamID, email, RoleAdmin)
	if err != nil {
		return false, err
	}
	if err := b.Mail.Send(ctx, []string{email}, "Your team's Paperclip is ready",
		"Your team's Paperclip collaboration agent is ready. Accept this one-time invite to become its first administrator:\n\n"+inv.URL.Reveal()+"\n"); err != nil {
		// A URL that nobody received is useless: withdraw it so the next run invites again.
		if rerr := b.Paper.RevokeInvite(ctx, c.TeamID, inv.ID); rerr != nil && b.Log != nil {
			b.Log.Warn("could not withdraw an unsent invite", "team", c.TeamID)
		}
		return false, fmt.Errorf("send the first admin invite: %w", err)
	}
	return true, nil
}

// Housekeeping is the database side of RC-27's table jobs.
type Housekeeping interface {
	// EnsureAccessPartition calls afs.ensure_collaboration_access_partition(month).
	EnsureAccessPartition(ctx context.Context, month string) error
	// PurgeEdgeSessions calls afs.purge_collab_edge_sessions().
	PurgeEdgeSessions(ctx context.Context) (int64, error)
}

// EnsurePartitions creates the collaboration_access partitions of the current month and the next one
// ahead (at least one month ahead, RC-27). month is the first day, YYYY-MM-01.
func EnsurePartitions(ctx context.Context, hk Housekeeping, now func() time.Time) error {
	t := now().UTC()
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 2; i++ {
		if err := hk.EnsureAccessPartition(ctx, first.AddDate(0, i, 0).Format("2006-01-02")); err != nil {
			return fmt.Errorf("collab: ensure partition: %w", err)
		}
	}
	return nil
}

// PurgeSessions deletes expired and revoked edge sessions and expired handoffs.
func PurgeSessions(ctx context.Context, hk Housekeeping, log *slog.Logger) error {
	n, err := hk.PurgeEdgeSessions(ctx)
	if err != nil {
		return fmt.Errorf("collab: purge edge sessions: %w", err)
	}
	if n > 0 && log != nil {
		log.Info("purged collaboration edge sessions", "rows", n)
	}
	return nil
}
