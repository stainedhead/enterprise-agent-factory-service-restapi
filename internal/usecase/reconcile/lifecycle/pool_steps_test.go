package lifecycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent/agenttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool/pooltest"
)

const (
	testPoolID  = "pool_01J0000000000000000000000P"
	testPoolID2 = "pool_01J0000000000000000000000Q"
	testTeamID  = "team_01J0000000000000000000000T"
)

type fakePoolWorkloads struct {
	l        *log
	stopped  bool
	stopErr  error
	checkErr error
}

func (w *fakePoolWorkloads) Stop(context.Context, pool.Desired) error {
	w.l.add("stop")
	return w.stopErr
}
func (w *fakePoolWorkloads) Stopped(context.Context, pool.Desired) (bool, error) {
	return w.stopped, w.checkErr
}

type fakePoolVolumes struct {
	l       *log
	gone    bool
	err     error
	goneErr error
}

func (v *fakePoolVolumes) DeleteVolumes(context.Context, pool.Desired) error {
	v.l.add("delete-volumes")
	return v.err
}
func (v *fakePoolVolumes) VolumesGone(context.Context, pool.Desired) (bool, error) {
	return v.gone, v.goneErr
}

type fakeQueueDeleter struct {
	l     *log
	names []string
	err   error
}

func (q *fakeQueueDeleter) Delete(_ context.Context, name string) error {
	q.l.add("delete-queue")
	q.names = append(q.names, name)
	return q.err
}

type poolFixture struct {
	f     *Finalizer
	l     *log
	src   *pooltest.Source
	work  *fakePoolWorkloads
	vols  *fakePoolVolumes
	roles *fakeRoles
	pods  *agenttest.PodIdentities
	queue *fakeQueueDeleter
	store *fakeStore
	comp  *fakeCompleter
}

func newPoolFixture(ps ...pool.Desired) *poolFixture {
	l := &log{}
	x := &poolFixture{
		l: l, src: pooltest.NewSource(ps...),
		work: &fakePoolWorkloads{l: l, stopped: true}, vols: &fakePoolVolumes{l: l, gone: true},
		roles: &fakeRoles{l: l}, pods: agenttest.NewPodIdentities(), queue: &fakeQueueDeleter{l: l},
		store: &fakeStore{}, comp: &fakeCompleter{l: l},
	}
	x.f = NewPoolFinalizer(PoolDeps{Source: x.src, Workloads: x.work, Volumes: x.vols, Pods: x.pods, IAM: x.roles, Queues: x.queue}, x.store, x.comp)
	return x
}

func deletingPool(id, name string) pool.Desired {
	return pool.Desired{
		ID: id, Name: name, State: pool.StateDeleting, Workers: 3, SharedVolume: pool.SharedEFS,
		Team: agent.Team{ID: testTeamID, Slug: "core", Namespace: "afs-core"},
	}
}

// M5-C4: a pool delete stops the workers, frees the volumes, then the identity and the inbox queue; the row is
// completed last.
func TestPoolDeleteStepsInOrder(t *testing.T) {
	x := newPoolFixture(deletingPool(testPoolID, "fixer"))
	res, err := x.f.Reconcile(context.Background(), PoolTarget(x.src.Pools[testPoolID]))
	if err != nil || !res.Done {
		t.Fatalf("Reconcile = %+v, %v; want done", res, err)
	}
	want := []string{"stop", "delete-volumes", "delete-role", "delete-queue", "complete"}
	if !slices.Equal(x.l.calls, want) {
		t.Errorf("calls = %v, want %v", x.l.calls, want)
	}
	if len(x.pods.Deleted) != 1 || x.pods.Deleted[0] != "afs-core/pool-fixer" {
		t.Errorf("pod identity deleted = %v", x.pods.Deleted)
	}
	if len(x.roles.paths) != 1 || x.roles.paths[0] != "/afs/afs-core/" || x.roles.names[0] != "afs-01J0000000000000000000000P" {
		t.Errorf("role deleted = %v %v", x.roles.paths, x.roles.names)
	}
	wantQueue, _ := pool.QueueName(testTeamID, testPoolID)
	if len(x.queue.names) != 1 || x.queue.names[0] != wantQueue {
		t.Errorf("queue deleted = %v, want %s", x.queue.names, wantQueue)
	}
	for _, typ := range []string{"DeleteStopWorkers", "DeleteDeleteVolumes", "DeletePodIdentity", "DeletePoolRole", "DeleteInboxQueue"} {
		if x.store.cond(typ).Status != "True" {
			t.Errorf("condition %s = %+v", typ, x.store.cond(typ))
		}
	}
	if got := PoolTarget(x.src.Pools[testPoolID]); got.Kind != "pool" || got.TeamID != testTeamID || got.Namespace != "afs-core" {
		t.Errorf("target = %+v", got)
	}
}

func TestPoolWorkersAndVolumesMustBeGoneInOrder(t *testing.T) {
	ctx := context.Background()
	x := newPoolFixture(deletingPool(testPoolID, "fixer"))
	tgt := PoolTarget(x.src.Pools[testPoolID])

	x.work.stopped = false
	res, err := x.f.Reconcile(ctx, tgt)
	if err != nil || res.Done || res.RequeueAfter == 0 {
		t.Fatalf("workers running: %+v, %v; want pending requeue", res, err)
	}
	if slices.Contains(x.l.calls, "delete-volumes") {
		t.Errorf("volumes touched while workers run: %v", x.l.calls)
	}
	x.work.stopped = true
	x.vols.gone = false
	if res, err = x.f.Reconcile(ctx, tgt); err != nil || res.Done {
		t.Fatalf("volumes terminating: %+v, %v", res, err)
	}
	if slices.Contains(x.l.calls, "delete-role") || slices.Contains(x.l.calls, "delete-queue") {
		t.Errorf("identity or queue removed before the volumes are gone: %v", x.l.calls)
	}
	x.vols.gone = true
	if res, err = x.f.Reconcile(ctx, tgt); err != nil || !res.Done {
		t.Fatalf("last pass = %+v, %v", res, err)
	}
	// Finished steps are not run again.
	if n := strings.Count(strings.Join(x.l.calls, ","), "stop"); n != 2 {
		t.Errorf("stop ran %d times, want 2 (the first two passes only)", n)
	}
}

func TestPoolStepErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		mut  func(*poolFixture)
	}{
		{"load pool", func(x *poolFixture) { x.src.Err = boom }},
		{"stop", func(x *poolFixture) { x.work.stopErr = boom }},
		{"stopped check", func(x *poolFixture) { x.work.checkErr = boom }},
		{"delete volumes", func(x *poolFixture) { x.vols.err = boom }},
		{"volumes check", func(x *poolFixture) { x.vols.goneErr = boom }},
		{"pod identity", func(x *poolFixture) { x.pods.Err = boom }},
		{"queue", func(x *poolFixture) { x.queue.err = boom }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := newPoolFixture(deletingPool(testPoolID, "fixer"))
			tc.mut(x)
			if _, err := x.f.Reconcile(context.Background(), PoolTarget(x.src.Pools[testPoolID])); err == nil {
				t.Fatal("want error")
			}
			if x.comp.n != 0 {
				t.Error("pool completed despite a failed step")
			}
		})
	}
	t.Run("pool id without a prefix", func(t *testing.T) {
		d := deletingPool("nope", "fixer")
		x := newPoolFixture(d)
		if _, err := x.f.Reconcile(context.Background(), PoolTarget(d)); err == nil || x.comp.n != 0 {
			t.Fatalf("err = %v completes %d", err, x.comp.n)
		}
	})
}

func TestPoolDeletingHook(t *testing.T) {
	ctx := context.Background()
	x := newPoolFixture(deletingPool(testPoolID, "fixer"))
	out, err := PoolDeleting(x.f)(ctx, x.src.Pools[testPoolID])
	if err != nil || out.Phase != "" || !out.NoStatus {
		t.Fatalf("done: %+v, %v", out, err)
	}
	y := newPoolFixture(deletingPool(testPoolID, "fixer"))
	y.work.stopped = false
	out, err = PoolDeleting(y.f)(ctx, y.src.Pools[testPoolID])
	if err != nil || out.Phase != "Deleting" || out.Requeue == 0 {
		t.Fatalf("pending: %+v, %v", out, err)
	}
	z := newPoolFixture(deletingPool(testPoolID, "fixer"))
	z.work.stopErr = errors.New("boom")
	out, err = PoolDeleting(z.f)(ctx, z.src.Pools[testPoolID])
	if err == nil || out.Phase != "Deleting" {
		t.Fatalf("failed: %+v, %v", out, err)
	}
}

// M5-C4: the team's delete runs every pool's own finalizer, and is pending while any is not done.
func TestTeamPoolsStep(t *testing.T) {
	ctx := context.Background()
	a, b := deletingPool(testPoolID, "fixer"), deletingPool(testPoolID2, "reviewer")
	x := newPoolFixture(a, b)
	step := TeamPoolsStep(x.src, x.f)
	if step.Name() != "Pools" {
		t.Errorf("step name = %s", step.Name())
	}
	tgt := Target{Kind: "team", ID: testTeamID, TeamID: testTeamID, Slug: "core"}

	x.work.stopped = false
	if err := step.Run(ctx, tgt); !errors.Is(err, ErrPending) {
		t.Fatalf("err = %v, want pending", err)
	}
	x.work.stopped = true
	if err := step.Run(ctx, tgt); err != nil {
		t.Fatalf("err = %v", err)
	}
	if x.comp.n != 2 {
		t.Errorf("pool completions = %d, want both pools", x.comp.n)
	}
	// (fakeStore keeps one set of conditions for every resource, so the second pool's steps are skipped here;
	// the order and the steps are proved per pool above.)

	t.Run("a failing pool is reported by name and does not hold the others", func(t *testing.T) {
		y := newPoolFixture(a, b)
		y.queue.err = errors.New("queue boom")
		err := TeamPoolsStep(y.src, y.f).Run(ctx, tgt)
		if err == nil || errors.Is(err, ErrPending) || !strings.Contains(err.Error(), "pool fixer") || !strings.Contains(err.Error(), "pool reviewer") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no pools: nothing to wait for", func(t *testing.T) {
		y := newPoolFixture()
		if err := TeamPoolsStep(y.src, y.f).Run(ctx, tgt); err != nil || y.comp.n != 0 {
			t.Fatalf("err = %v completes %d", err, y.comp.n)
		}
	})
	t.Run("list error", func(t *testing.T) {
		y := newPoolFixture(a)
		y.src.Err = errors.New("db boom")
		if err := TeamPoolsStep(y.src, y.f).Run(ctx, tgt); err == nil {
			t.Fatal("want error")
		}
	})
}

// The team finalizer runs the pool step after the agents and before the team's own steps (secrets, IAM,
// namespace), so a pool's role and queue go before the team's role and namespace.
func TestPoolStepRunsBeforeTeamSteps(t *testing.T) {
	x := newPoolFixture(deletingPool(testPoolID, "fixer"))
	f, _, _, _, l := newFixture()
	pools := TeamPoolsStep(x.src, x.f)
	f.Steps = TeamSteps(TeamDeps{Secrets: &fakeAWS{l: l}, IAM: &fakeAWS{l: l}, Profiles: &fakeAWS{l: l}, Access: &fakeAWS{l: l}, NS: &fakeAWS{l: l, nsGoneAt: 1}}, Hooks{Pools: []Step{pools}})
	if f.Steps[0].Name() != "Pools" {
		t.Errorf("first step = %s", f.Steps[0].Name())
	}
}
