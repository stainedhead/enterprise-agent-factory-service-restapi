package snapshot_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent/agenttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot/snapshottest"
)

const (
	teamID = "team_01JD0000000000000000000011"
	agtA   = "agt_01JD0000000000000000000021"
	agtB   = "agt_01JD0000000000000000000022"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	clk   *clock
	store *snapshottest.Store
	cl    *snapshottest.Cluster
	vols  *agenttest.Volumes
	src   *agenttest.Source
	mgr   *snapshot.Manager
}

func agentOf(id, name, schedule string, retain int) agent.Desired {
	return agent.Desired{ID: id, Name: name, State: agent.StateEnabled,
		Volume: agent.Volume{SizeGiB: 20, SnapshotSchedule: schedule, SnapshotRetain: retain},
		Team:   agent.Team{ID: teamID, Namespace: "afs-t-x"}}
}

func newRig(agents ...agent.Desired) *rig {
	r := &rig{clk: &clock{t: time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)}, cl: snapshottest.NewCluster(), vols: agenttest.NewVolumes(), src: agenttest.NewSource(agents...)}
	r.store = snapshottest.NewStore(r.clk.Now)
	for _, a := range agents {
		r.store.Schedules[a.ID] = a.Volume.SnapshotSchedule
		r.vols.States[a.ID] = agent.VolumeState{Exists: true, Bound: true, RequestedGiB: 20, CapacityGiB: 20}
	}
	r.mgr = &snapshot.Manager{Store: r.store, Agents: r.src, Volumes: r.vols, Cluster: r.cl, Name: func(id string) string { return "n-" + id }, Now: r.clk.Now}
	return r
}

func (r *rig) run(t *testing.T) {
	t.Helper()
	if err := r.mgr.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func count(r *rig, agentID, reason string) int {
	n := 0
	for _, row := range r.store.Rows {
		if row.AgentID == agentID && row.Reason == reason {
			n++
		}
	}
	return n
}

// M2-G2: a daily schedule requests a snapshot when none exists, not again within 24 h, and again after.
func TestScheduleDaily(t *testing.T) {
	r := newRig(agentOf(agtA, "a", "daily", 7))
	r.run(t)
	if count(r, agtA, "scheduled") != 1 {
		t.Fatalf("first pass requested %d snapshots", count(r, agtA, "scheduled"))
	}
	r.clk.Add(23 * time.Hour)
	r.run(t)
	if count(r, agtA, "scheduled") != 1 {
		t.Errorf("23 h later: %d snapshots, want 1", count(r, agtA, "scheduled"))
	}
	r.clk.Add(time.Hour)
	r.run(t)
	if count(r, agtA, "scheduled") != 2 {
		t.Errorf("24 h later: %d snapshots, want 2", count(r, agtA, "scheduled"))
	}
}

func TestScheduleWeeklyAndOff(t *testing.T) {
	r := newRig(agentOf(agtA, "a", "weekly", 7), agentOf(agtB, "b", "off", 7))
	r.run(t)
	r.clk.Add(6 * 24 * time.Hour)
	r.run(t)
	if count(r, agtA, "scheduled") != 1 {
		t.Errorf("weekly after 6 days: %d", count(r, agtA, "scheduled"))
	}
	r.clk.Add(24 * time.Hour)
	r.run(t)
	if count(r, agtA, "scheduled") != 2 {
		t.Errorf("weekly after 7 days: %d", count(r, agtA, "scheduled"))
	}
	if count(r, agtB, "scheduled") != 0 {
		t.Error("a schedule that is off requested a snapshot")
	}
	if snapshot.Interval("off") != 0 || snapshot.Interval("daily") != 24*time.Hour {
		t.Error("intervals")
	}
}

// Nothing is requested for an agent whose volume does not exist yet, or that is being deleted.
func TestNoSnapshotWithoutVolume(t *testing.T) {
	a := agentOf(agtA, "a", "daily", 7)
	r := newRig(a, agentOf(agtB, "b", "daily", 7))
	r.vols.States[agtA] = agent.VolumeState{}
	r.vols.States[agtB] = agent.VolumeState{Exists: true, RequestedGiB: 20} // not bound yet
	r.run(t)
	if len(r.store.Rows) != 0 {
		t.Errorf("rows = %d", len(r.store.Rows))
	}
	b := agentOf(agtB, "b", "daily", 7)
	b.State = agent.StateDeleting
	r.src.Put(b)
	r.vols.States[agtB] = agent.VolumeState{Exists: true, Bound: true, RequestedGiB: 20}
	r.run(t)
	if len(r.store.Rows) != 0 {
		t.Errorf("a deleting agent was snapshotted: %d rows", len(r.store.Rows))
	}
}

// A row goes requested, pending (once AWS has a snapshot id) and completed (once ready); a manual request made
// through the API row is picked up like a scheduled one.
func TestLifecycleOfARow(t *testing.T) {
	r := newRig(agentOf(agtA, "a", "off", 7))
	r.store.Add(snapshot.Row{ID: "vsnap_M", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "requested", RequestedAt: r.clk.Now()})
	r.run(t)
	if !r.cl.Requested["n-vsnap_M"] || len(r.cl.Prepared) != 1 {
		t.Fatalf("manual request not sent: %v %v", r.cl.Requested, r.cl.Prepared)
	}
	if st := r.store.States()["vsnap_M"]; st != "requested" {
		t.Errorf("state = %s before the driver has an id", st)
	}
	r.cl.Handle("n-vsnap_M", "snap-0123456789abcdef0")
	r.run(t)
	if st := r.store.States()["vsnap_M"]; st != "pending" || r.store.Rows[0].EBSID != "snap-0123456789abcdef0" {
		t.Errorf("state = %s ebs %s", st, r.store.Rows[0].EBSID)
	}
	r.cl.Complete("n-vsnap_M", "snap-0123456789abcdef0", 20)
	r.run(t)
	if st := r.store.States()["vsnap_M"]; st != "completed" || r.store.Rows[0].SizeGiB != 20 {
		t.Errorf("state = %s size %d", st, r.store.Rows[0].SizeGiB)
	}
	if len(r.cl.Prepared) != 2 {
		t.Errorf("the class was prepared %d times; only the two passes with a requested row prepare it", len(r.cl.Prepared))
	}
}

func TestFailedAndStuckSnapshots(t *testing.T) {
	r := newRig(agentOf(agtA, "a", "off", 7))
	r.store.Add(snapshot.Row{ID: "vsnap_F", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "requested", RequestedAt: r.clk.Now()})
	r.run(t)
	r.cl.Fail("n-vsnap_F", "the volume is not attached")
	r.run(t)
	if st := r.store.States()["vsnap_F"]; st != "failed" {
		t.Errorf("failed driver: state = %s", st)
	}
	r.store.Add(snapshot.Row{ID: "vsnap_S", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "requested", RequestedAt: r.clk.Now()})
	r.run(t)
	r.clk.Add(snapshot.StuckAfter + time.Minute)
	r.run(t)
	if st := r.store.States()["vsnap_S"]; st != "failed" {
		t.Errorf("stuck: state = %s", st)
	}
}

// M2-G2: completed snapshots beyond snapshotRetain are deleted (cluster and database), the newest are kept,
// and the final snapshot of a delete is never pruned.
func TestPruneToRetain(t *testing.T) {
	r := newRig(agentOf(agtA, "a", "daily", 3))
	base := r.clk.Now()
	for i := 1; i <= 5; i++ {
		r.store.Add(snapshot.Row{ID: fmt.Sprintf("vsnap_%d", i), TeamID: teamID, AgentID: agtA, Reason: "scheduled", State: "completed",
			EBSID: fmt.Sprintf("snap-%017x", i), RequestedAt: base.Add(-time.Duration(10-i) * time.Hour)})
	}
	r.store.Add(snapshot.Row{ID: "vsnap_final", TeamID: teamID, AgentID: agtA, Reason: "pre_delete", State: "completed", EBSID: "snap-0000000000000fff", RequestedAt: base.Add(-100 * time.Hour)})
	r.store.Add(snapshot.Row{ID: "vsnap_manual", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "completed", EBSID: "snap-0000000000000eee", RequestedAt: base.Add(-time.Hour)})
	r.run(t)
	st := r.store.States()
	// Retain 3 of scheduled + manual, newest first: manual, 5, 4 stay; 3, 2, 1 go.
	for _, id := range []string{"vsnap_manual", "vsnap_5", "vsnap_4"} {
		if st[id] != "completed" {
			t.Errorf("%s = %s, want kept", id, st[id])
		}
	}
	for _, id := range []string{"vsnap_3", "vsnap_2", "vsnap_1"} {
		if st[id] != "deleted" {
			t.Errorf("%s = %s, want deleted", id, st[id])
		}
	}
	if st["vsnap_final"] != "completed" {
		t.Error("the final snapshot was pruned")
	}
	if len(r.cl.Deleted) != 3 {
		t.Errorf("cluster deletes = %v", r.cl.Deleted)
	}
	// Lowering the retention prunes on the next pass.
	a := agentOf(agtA, "a", "daily", 1)
	r.src.Put(a)
	r.run(t)
	if r.store.States()["vsnap_5"] != "deleted" || r.store.States()["vsnap_manual"] != "completed" {
		t.Errorf("after lowering: %v", r.store.States())
	}
}

func TestLatestReady(t *testing.T) {
	r := newRig(agentOf(agtA, "a", "off", 7))
	l := snapshot.Latest{Store: r.store}
	if _, ok, err := l.LatestReady(t.Context(), agtA); ok || err != nil {
		t.Errorf("no snapshot: %v %v", ok, err)
	}
	r.store.Add(snapshot.Row{ID: "vsnap_1", AgentID: agtA, State: "completed", EBSID: "snap-0000000000000001", RequestedAt: r.clk.Now().Add(-time.Hour)})
	r.store.Add(snapshot.Row{ID: "vsnap_2", AgentID: agtA, State: "completed", EBSID: "snap-0000000000000002", RequestedAt: r.clk.Now()})
	r.store.Add(snapshot.Row{ID: "vsnap_3", AgentID: agtA, State: "failed", RequestedAt: r.clk.Now().Add(time.Hour)})
	if id, ok, _ := l.LatestReady(t.Context(), agtA); !ok || id != "snap-0000000000000002" {
		t.Errorf("latest = %q %v", id, ok)
	}
}

func TestClusterErrorsAreReported(t *testing.T) {
	r := newRig(agentOf(agtA, "a", "off", 7))
	r.store.Add(snapshot.Row{ID: "vsnap_E", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "requested", RequestedAt: r.clk.Now()})
	r.cl.Err = fmt.Errorf("forbidden")
	if err := r.mgr.Run(t.Context()); err == nil {
		t.Error("cluster error swallowed")
	}
	if st := r.store.States()["vsnap_E"]; st != "requested" {
		t.Errorf("a transient cluster error changed the row to %s", st)
	}
}

// failing wraps the fake store and fails the named operation.
type failing struct {
	*snapshottest.Store
	op string
}

var errStore = fmt.Errorf("database down")

func (f failing) fail(op string) error {
	if f.op == op {
		return errStore
	}
	return nil
}

func (f failing) Scheduled(ctx context.Context) ([]snapshot.Scheduled, error) {
	if err := f.fail("Scheduled"); err != nil {
		return nil, err
	}
	return f.Store.Scheduled(ctx)
}

func (f failing) Open(ctx context.Context) ([]snapshot.Row, error) {
	if err := f.fail("Open"); err != nil {
		return nil, err
	}
	return f.Store.Open(ctx)
}

func (f failing) Completed(ctx context.Context, id string) ([]snapshot.Row, error) {
	if err := f.fail("Completed"); err != nil {
		return nil, err
	}
	return f.Store.Completed(ctx, id)
}

func (f failing) Create(ctx context.Context, team, id, reason string) (string, error) {
	if err := f.fail("Create"); err != nil {
		return "", err
	}
	return f.Store.Create(ctx, team, id, reason)
}

func (f failing) MarkCompleted(ctx context.Context, id, ebs string, size int, at time.Time) error {
	if err := f.fail("MarkCompleted"); err != nil {
		return err
	}
	return f.Store.MarkCompleted(ctx, id, ebs, size, at)
}

func (f failing) MarkDeleted(ctx context.Context, id string) error {
	if err := f.fail("MarkDeleted"); err != nil {
		return err
	}
	return f.Store.MarkDeleted(ctx, id)
}

func TestStoreAndSourceFailuresAreReportedAndIsolated(t *testing.T) {
	for _, op := range []string{"Scheduled", "Open", "Completed", "Create", "MarkCompleted", "MarkDeleted"} {
		r := newRig(agentOf(agtA, "a", "daily", 1))
		r.store.Add(snapshot.Row{ID: "vsnap_1", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "pending", EBSID: "snap-0000000000000001", RequestedAt: r.clk.Now()})
		r.store.Add(snapshot.Row{ID: "vsnap_2", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "completed", EBSID: "snap-0000000000000002", RequestedAt: r.clk.Now().Add(-time.Hour)})
		r.store.Add(snapshot.Row{ID: "vsnap_3", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "completed", EBSID: "snap-0000000000000003", RequestedAt: r.clk.Now().Add(-2 * time.Hour)})
		r.cl.Complete("n-vsnap_1", "snap-0000000000000001", 20)
		r.mgr.Store = failing{Store: r.store, op: op}
		if err := r.mgr.Run(t.Context()); err == nil {
			t.Errorf("%s failure swallowed", op)
		}
	}
	// An agent that cannot be loaded does not stop the others.
	r := newRig(agentOf(agtA, "a", "daily", 7), agentOf(agtB, "b", "daily", 7))
	delete(r.src.Agents, agtA)
	if err := r.mgr.Run(t.Context()); err == nil {
		t.Error("missing agent not reported")
	}
	if count(r, agtB, "scheduled") != 1 {
		t.Error("one agent's failure stopped the others")
	}
	r = newRig(agentOf(agtA, "a", "daily", 7))
	r.vols.Err = fmt.Errorf("forbidden")
	if err := r.mgr.Run(t.Context()); err == nil {
		t.Error("volume error not reported")
	}
}

func TestDefaultsAndLogging(t *testing.T) {
	r := newRig(agentOf(agtA, "a", "off", 7))
	r.mgr.Name, r.mgr.Now = nil, nil
	r.mgr.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	r.store.Add(snapshot.Row{ID: "vsnap_L", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "requested", RequestedAt: time.Now().Add(-2 * snapshot.StuckAfter)})
	r.run(t)
	if st := r.store.States()["vsnap_L"]; st != "failed" || !r.cl.Requested["vsnap_L"] {
		t.Errorf("state %s requested %v (default name is the row id)", st, r.cl.Requested)
	}
	r.store.Add(snapshot.Row{ID: "vsnap_M", TeamID: teamID, AgentID: agtA, Reason: "manual", State: "requested", RequestedAt: time.Now()})
	r.cl.Fail("vsnap_M", "boom")
	r.run(t)
	if st := r.store.States()["vsnap_M"]; st != "failed" {
		t.Errorf("driver error: %s", st)
	}
}
