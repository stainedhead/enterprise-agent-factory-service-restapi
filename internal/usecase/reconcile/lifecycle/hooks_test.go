package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	teamctl "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

var desired = teamctl.Desired{ID: "team_01", Slug: "alpha", Namespace: "afs-t-alpha", State: "deleting", Generation: 4}

// M1-I1 wiring: the team controller's Deleting hook drives the finalizer and tells the engine to
// leave resource_status to it (the finalizer writes the step conditions) while keeping Deleting.
func TestTeamDeletingHook(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		f, _, _, c, _ := newFixture()
		out, err := TeamDeleting(f)(context.Background(), desired)
		if err != nil || !out.NoStatus || c.n != 1 {
			t.Fatalf("out %+v err %v completes %d", out, err, c.n)
		}
	})
	t.Run("namespace still terminating requeues without an error", func(t *testing.T) {
		f, a, _, _, _ := newFixture()
		a.nsGoneAt = 99
		out, err := TeamDeleting(f)(context.Background(), desired)
		if err != nil || !out.NoStatus || out.Requeue != PendingRequeue || out.Phase != domain.PhaseDeleting {
			t.Fatalf("out %+v err %v", out, err)
		}
	})
	t.Run("a failed step returns the error with phase Deleting", func(t *testing.T) {
		f, a, _, _, _ := newFixture()
		a.revokeErr = errors.New("boom")
		out, err := TeamDeleting(f)(context.Background(), desired)
		if err == nil || out.Phase != domain.PhaseDeleting || !out.NoStatus {
			t.Fatalf("out %+v err %v", out, err)
		}
	})
	t.Run("target is built from the team", func(t *testing.T) {
		got := TeamTarget(desired)
		want := Target{Kind: "team", ID: "team_01", TeamID: "team_01", Slug: "alpha", Namespace: "afs-t-alpha"}
		if got != want {
			t.Errorf("target = %+v", got)
		}
	})
}
