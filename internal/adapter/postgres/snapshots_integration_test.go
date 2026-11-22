package postgres_test

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot"
)

// M2-G2: the snapshot rows move requested → pending → completed with the grants the reconciler has, and
// the schedule query reports the agent's latest scheduled snapshot.
func TestSnapshotStore(t *testing.T) {
	db := pgtest.Get(t)
	s := seedAgent(t, db)
	runner, _ := reconcilerRunner(t, db)
	st := postgres.NewSnapshotStore(runner)
	ctx := t.Context()
	admin := db.Admin(t)
	if _, err := admin.Exec(ctx, `UPDATE afs.agents SET snapshot_schedule = 'daily' WHERE id = $1`, s.agentID); err != nil {
		t.Fatal(err)
	}

	sched, err := st.Scheduled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *snapshot.Scheduled
	for i := range sched {
		if sched[i].AgentID == s.agentID {
			found = &sched[i]
		}
	}
	if found == nil || found.Schedule != "daily" || found.Last != nil {
		t.Fatalf("Scheduled = %+v", sched)
	}

	id, err := st.Create(ctx, s.team.ID, s.agentID, snapshot.ReasonScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if open, _ := st.Open(ctx); !containsRow(open, id, snapshot.StateRequested) {
		t.Fatalf("Open = %+v", open)
	}
	if err := st.MarkPending(ctx, id, "snap-0123456789abcdef0"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCompleted(ctx, id, "snap-0123456789abcdef0", 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	done, err := st.Completed(ctx, s.agentID)
	if err != nil || len(done) != 1 || done[0].EBSID != "snap-0123456789abcdef0" || done[0].SizeGiB != 30 {
		t.Fatalf("Completed = %+v, %v", done, err)
	}
	sched, _ = st.Scheduled(ctx)
	for _, x := range sched {
		if x.AgentID == s.agentID && x.Last == nil {
			t.Error("latest scheduled snapshot not reported")
		}
	}

	failed, _ := st.Create(ctx, s.team.ID, s.agentID, snapshot.ReasonPreDelete)
	if err := st.MarkFailed(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.ForAgent(ctx, s.agentID, snapshot.ReasonPreDelete); !containsRow(rows, failed, snapshot.StateFailed) {
		t.Errorf("ForAgent = %+v", rows)
	}
	if err := st.MarkDeleted(ctx, id); err != nil {
		t.Fatal(err)
	}
	if done, _ := st.Completed(ctx, s.agentID); len(done) != 0 {
		t.Errorf("deleted row still completed: %+v", done)
	}
	if err := st.MarkFailed(ctx, "vsnap_01JD0000000000000000000099"); err == nil {
		t.Error("updating a missing row succeeded")
	}
}

func containsRow(rows []snapshot.Row, id, state string) bool {
	for _, r := range rows {
		if r.ID == id && r.State == state {
			return true
		}
	}
	return false
}

// M2-G4: the agent finalizer's last step and the retained-volume record.
func TestAgentLifecycleComplete(t *testing.T) {
	db := pgtest.Get(t)
	runner, audit := reconcilerRunner(t, db)
	lc := postgres.NewAgentLifecycle(runner, audit)
	ctx := t.Context()
	admin := db.Admin(t)
	count := func(q string, args ...any) int {
		var n int
		if err := admin.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	s := seedAgent(t, db)
	tgt := lifecycle.Target{Kind: "agent", ID: s.agentID, TeamID: s.team.ID}
	if err := lc.Complete(ctx, tgt); err == nil {
		t.Fatal("an enabled agent was finalized")
	}
	if _, err := admin.Exec(ctx, `UPDATE afs.agents SET state = 'deleting' WHERE id = $1`, s.agentID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := lc.Complete(ctx, tgt); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if n := count(`SELECT count(*) FROM afs.agents WHERE id = $1 AND state = 'deleted' AND deleted_at IS NOT NULL`, s.agentID); n != 1 {
		t.Fatal("agent not deleted")
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action = 'agent.delete_finalized'`, s.agentID); n != 1 {
		t.Fatalf("audit records = %d, want 1", n)
	}
	if err := lc.Complete(ctx, lifecycle.Target{Kind: "team", ID: s.team.ID}); err == nil {
		t.Error("a team was finalized as an agent")
	}

	d := agent.Desired{ID: s.agentID, Team: agent.Team{ID: s.team.ID, Namespace: "afs-x"}}
	if err := lc.RecordRetained(ctx, d, lifecycle.RetainedVolume{Claim: "data-agent-reviewer-0", Volume: "pvc-1"}); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action = 'agent.volume_retained' AND after->>'volume' = 'pvc-1'`, s.agentID); n != 1 {
		t.Fatalf("retained records = %d", n)
	}
}
