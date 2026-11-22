package collab

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// BoardKeyCandidate is a collaboration agent whose board key is not recorded yet.
type BoardKeyCandidate struct {
	TeamID, AgentID string
	Mode            domain.RegistrationMode
}

// BoardKeys is the bootstrap's view of the board key (RC-29). In managed mode the pod's entrypoint writes the
// key to Secrets Manager; the reconciler reads only that name, never a value.
type BoardKeys interface {
	// Pending returns the collaboration agents in managed mode without a recorded board key.
	Pending(ctx context.Context) ([]BoardKeyCandidate, error)
	// Find returns the ARN of secret <service>/<namespace>/<agent>/paperclip-board-api-key when it exists.
	Find(ctx context.Context, teamID, agentID string) (arn string, found bool, err error)
	// Record calls afs.record_collab_board_key.
	Record(ctx context.Context, teamID, agentID, arn string) error
}

// Bootstrap records the board key and announces the end of the bootstrap once (collaboration.bootstrap_completed).
type Bootstrap struct {
	Keys   BoardKeys
	Events Events
	Log    *slog.Logger
}

// Run checks once. Recording is idempotent, so a crash between the record and the event repeats the event at
// most once more: the candidate list no longer holds the agent after the record.
func (b *Bootstrap) Run(ctx context.Context) error {
	cands, err := b.Keys.Pending(ctx)
	if err != nil {
		return fmt.Errorf("collab: pending board keys: %w", err)
	}
	var failed int
	for _, c := range cands {
		arn, found, err := b.Keys.Find(ctx, c.TeamID, c.AgentID)
		if err != nil {
			failed++
			continue
		}
		if !found {
			continue
		}
		if err := b.Keys.Record(ctx, c.TeamID, c.AgentID, arn); err != nil {
			failed++
			continue
		}
		// The event carries no invite URL and no key: the invite is a credential (G8).
		if err := b.Events.Publish(ctx, domain.EventCollaborationBootstrapCompleted, c.TeamID, string(KindTeamCollaboration), c.TeamID, c.AgentID,
			map[string]any{"teamId": c.TeamID, "agentId": c.AgentID, "mode": string(c.Mode)}); err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: %d bootstraps failed", failed)
	}
	return nil
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
