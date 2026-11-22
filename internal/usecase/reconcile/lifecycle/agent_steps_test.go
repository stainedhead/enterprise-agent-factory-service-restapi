package lifecycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent/agenttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot/snapshottest"
)

const testAgentID = "agt_01J0000000000000000000000A"

type fakeWorkloads struct {
	l        *log
	stopped  bool
	stopErr  error
	checkErr error
}

func (w *fakeWorkloads) Stop(context.Context, agent.Desired) error {
	w.l.add("stop")
	return w.stopErr
}
func (w *fakeWorkloads) Stopped(context.Context, agent.Desired) (bool, error) {
	return w.stopped, w.checkErr
}

type fakeVolumes struct {
	l        *log
	retained RetainedVolume
	err      error
}

func (v *fakeVolumes) DeleteVolume(context.Context, agent.Desired) error {
	v.l.add("delete-volume")
	return v.err
}
func (v *fakeVolumes) RetainVolume(context.Context, agent.Desired) (RetainedVolume, error) {
	v.l.add("retain-volume")
	return v.retained, v.err
}
func (v *fakeVolumes) DeleteSnapshotClass(context.Context, agent.Desired) error {
	v.l.add("delete-class")
	return nil
}

type fakeRecorder struct {
	l    *log
	got  []RetainedVolume
	fail error
}

func (r *fakeRecorder) RecordRetained(_ context.Context, _ agent.Desired, v RetainedVolume) error {
	r.l.add("record")
	r.got = append(r.got, v)
	return r.fail
}

type fakeRoles struct {
	l     *log
	paths []string
	names []string
}

func (r *fakeRoles) DeleteRole(_ context.Context, path, name string) error {
	r.l.add("delete-role")
	r.paths, r.names = append(r.paths, path), append(r.names, name)
	return nil
}

type agentFixture struct {
	f     *Finalizer
	l     *log
	d     AgentDeps
	src   *agenttest.Source
	work  *fakeWorkloads
	vols  *fakeVolumes
	rec   *fakeRecorder
	roles *fakeRoles
	pods  *agenttest.PodIdentities
	snaps *snapshottest.Store
	store *fakeStore
	comp  *fakeCompleter
}

func newAgentFixture(a agent.Desired) *agentFixture {
	l := &log{}
	x := &agentFixture{
		l:     l,
		src:   agenttest.NewSource(a),
		work:  &fakeWorkloads{l: l, stopped: true},
		vols:  &fakeVolumes{l: l},
		rec:   &fakeRecorder{l: l},
		roles: &fakeRoles{l: l},
		pods:  agenttest.NewPodIdentities(),
		snaps: snapshottest.NewStore(time.Now),
		store: &fakeStore{},
		comp:  &fakeCompleter{l: l},
	}
	x.d = AgentDeps{Source: x.src, Workloads: x.work, Volumes: x.vols, Snapshots: x.snaps, Recorder: x.rec, Pods: x.pods, IAM: x.roles}
	x.f = NewAgentFinalizer(x.d, x.store, x.comp)
	return x
}

func deletingAgent(onDelete, identity string) agent.Desired {
	return agent.Desired{
		ID: testAgentID, Name: "scribe", State: agent.StateDeleting, IAMIdentity: identity,
		Volume: agent.Volume{OnDelete: onDelete},
		Team:   agent.Team{ID: "team_01J0000000000000000000000T", Slug: "core", Namespace: "afs-core"},
	}
}

func TestAgentDeleteRules(t *testing.T) {
	tests := []struct {
		name     string
		onDelete string
		identity string
		prep     func(*agentFixture)
		want     []string
		absent   []string
	}{
		{name: "delete removes the claim", onDelete: OnDeleteDelete, identity: agent.IdentityTeamShared,
			want: []string{"stop", "delete-volume", "delete-class", "complete"}, absent: []string{"retain-volume", "delete-role"}},
		{name: "retain keeps and records the volume", onDelete: OnDeleteRetain, identity: agent.IdentityTeamShared,
			prep: func(x *agentFixture) { x.vols.retained = RetainedVolume{Claim: "data-agent-scribe-0", Volume: "pvc-1"} },
			want: []string{"stop", "retain-volume", "record", "delete-class", "complete"}, absent: []string{"delete-volume"}},
		{name: "dedicated role is deleted", onDelete: OnDeleteDelete, identity: agent.IdentityDedicated,
			want: []string{"stop", "delete-volume", "delete-role", "delete-class", "complete"}},
		{name: "snapshot then delete after a completed final snapshot", onDelete: OnDeleteSnapshotThenDelete, identity: agent.IdentityTeamShared,
			prep: func(x *agentFixture) {
				x.snaps.Add(snapshot.Row{ID: "vsnap_1", AgentID: testAgentID, Reason: snapshot.ReasonPreDelete, State: snapshot.StateCompleted})
			},
			want: []string{"stop", "delete-volume", "delete-class", "complete"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := newAgentFixture(deletingAgent(tc.onDelete, tc.identity))
			if tc.prep != nil {
				tc.prep(x)
			}
			res, err := x.f.Reconcile(context.Background(), AgentTarget(x.src.Agents[testAgentID]))
			if err != nil || !res.Done {
				t.Fatalf("Reconcile = %+v, %v; want done", res, err)
			}
			for _, w := range tc.want {
				if !slices.Contains(x.l.calls, w) {
					t.Errorf("missing call %q in %v", w, x.l.calls)
				}
			}
			for _, a := range tc.absent {
				if slices.Contains(x.l.calls, a) {
					t.Errorf("unexpected call %q in %v", a, x.l.calls)
				}
			}
			if x.l.calls[len(x.l.calls)-1] != "complete" {
				t.Errorf("complete must be last: %v", x.l.calls)
			}
			if got := len(x.pods.Deleted); got != 1 || x.pods.Deleted[0] != "afs-core/"+agent.ServiceAccountName("scribe") {
				t.Errorf("pod identity deleted = %v", x.pods.Deleted)
			}
			if tc.identity == agent.IdentityDedicated {
				if len(x.roles.paths) != 1 || x.roles.paths[0] != "/afs/afs-core/" || !strings.HasPrefix(x.roles.names[0], "afs-") {
					t.Errorf("role deleted = %v %v", x.roles.paths, x.roles.names)
				}
			}
		})
	}
}

func TestRetainRecordsTheVolume(t *testing.T) {
	x := newAgentFixture(deletingAgent(OnDeleteRetain, agent.IdentityTeamShared))
	x.vols.retained = RetainedVolume{Claim: "data-agent-scribe-0", Volume: "pvc-1"}
	if _, err := x.f.Reconcile(context.Background(), AgentTarget(x.src.Agents[testAgentID])); err != nil {
		t.Fatal(err)
	}
	if len(x.rec.got) != 1 || x.rec.got[0].Volume != "pvc-1" {
		t.Errorf("recorded = %+v", x.rec.got)
	}
	// A missing claim has nothing to record.
	y := newAgentFixture(deletingAgent(OnDeleteRetain, agent.IdentityTeamShared))
	if _, err := y.f.Reconcile(context.Background(), AgentTarget(y.src.Agents[testAgentID])); err != nil {
		t.Fatal(err)
	}
	if len(y.rec.got) != 0 {
		t.Errorf("recorded = %+v, want none", y.rec.got)
	}
}

func TestWorkloadMustBeGoneBeforeTheVolumeIsTouched(t *testing.T) {
	x := newAgentFixture(deletingAgent(OnDeleteDelete, agent.IdentityTeamShared))
	x.work.stopped = false
	res, err := x.f.Reconcile(context.Background(), AgentTarget(x.src.Agents[testAgentID]))
	if err != nil || res.Done || res.RequeueAfter == 0 {
		t.Fatalf("Reconcile = %+v, %v; want pending requeue", res, err)
	}
	if slices.Contains(x.l.calls, "delete-volume") {
		t.Errorf("volume touched while pods run: %v", x.l.calls)
	}
	x.work.stopped = true
	if res, err = x.f.Reconcile(context.Background(), AgentTarget(x.src.Agents[testAgentID])); err != nil || !res.Done {
		t.Fatalf("second pass = %+v, %v", res, err)
	}
}

func TestFinalSnapshotLifecycle(t *testing.T) {
	x := newAgentFixture(deletingAgent(OnDeleteSnapshotThenDelete, agent.IdentityTeamShared))
	tgt := AgentTarget(x.src.Agents[testAgentID])
	ctx := context.Background()

	// 1. none yet: one is requested and the delete waits.
	res, err := x.f.Reconcile(ctx, tgt)
	if err != nil || res.Done || res.RequeueAfter == 0 {
		t.Fatalf("pass 1 = %+v, %v", res, err)
	}
	if len(x.snaps.Rows) != 1 || x.snaps.Rows[0].Reason != snapshot.ReasonPreDelete {
		t.Fatalf("rows = %+v", x.snaps.Rows)
	}
	// 2. still requested: no second snapshot, volume untouched.
	if res, err = x.f.Reconcile(ctx, tgt); err != nil || res.Done {
		t.Fatalf("pass 2 = %+v, %v", res, err)
	}
	if len(x.snaps.Rows) != 1 || slices.Contains(x.l.calls, "delete-volume") {
		t.Fatalf("rows = %d calls = %v", len(x.snaps.Rows), x.l.calls)
	}
	// 3. it fails: an error, and another is requested; the volume is still kept.
	_ = x.snaps.MarkFailed(ctx, x.snaps.Rows[0].ID)
	if _, err = x.f.Reconcile(ctx, tgt); err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("pass 3 err = %v, want a failure", err)
	}
	if len(x.snaps.Rows) != 2 || slices.Contains(x.l.calls, "delete-volume") {
		t.Fatalf("rows = %d calls = %v", len(x.snaps.Rows), x.l.calls)
	}
	// 4. the retry completes: the claim is deleted and the agent completes.
	_ = x.snaps.MarkCompleted(ctx, x.snaps.Rows[1].ID, "snap-0abc", 10, time.Now())
	if res, err = x.f.Reconcile(ctx, tgt); err != nil || !res.Done {
		t.Fatalf("pass 4 = %+v, %v", res, err)
	}
	if !slices.Contains(x.l.calls, "delete-volume") {
		t.Errorf("claim not deleted: %v", x.l.calls)
	}
}

func TestAgentStepErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		mut  func(*agentFixture)
	}{
		{"load agent", func(x *agentFixture) { x.src.Err = boom }},
		{"stop", func(x *agentFixture) { x.work.stopErr = boom }},
		{"stopped check", func(x *agentFixture) { x.work.checkErr = boom }},
		{"delete volume", func(x *agentFixture) { x.vols.err = boom }},
		{"pod identity", func(x *agentFixture) { x.pods.Err = boom }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := newAgentFixture(deletingAgent(OnDeleteDelete, agent.IdentityTeamShared))
			tc.mut(x)
			if _, err := x.f.Reconcile(context.Background(), AgentTarget(x.src.Agents[testAgentID])); err == nil {
				t.Fatal("want error")
			}
			if x.comp.n != 0 {
				t.Error("agent completed despite a failed step")
			}
		})
	}
	t.Run("retain error and recorder error", func(t *testing.T) {
		x := newAgentFixture(deletingAgent(OnDeleteRetain, agent.IdentityTeamShared))
		x.vols.err = boom
		if _, err := x.f.Reconcile(context.Background(), AgentTarget(x.src.Agents[testAgentID])); err == nil {
			t.Fatal("want error")
		}
		y := newAgentFixture(deletingAgent(OnDeleteRetain, agent.IdentityTeamShared))
		y.vols.retained = RetainedVolume{Volume: "pvc-1"}
		y.rec.fail = boom
		if _, err := y.f.Reconcile(context.Background(), AgentTarget(y.src.Agents[testAgentID])); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("unknown rule", func(t *testing.T) {
		x := newAgentFixture(deletingAgent("shred", agent.IdentityTeamShared))
		if _, err := x.f.Reconcile(context.Background(), AgentTarget(x.src.Agents[testAgentID])); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestAgentDeletingHook(t *testing.T) {
	x := newAgentFixture(deletingAgent(OnDeleteDelete, agent.IdentityTeamShared))
	hook := AgentDeleting(x.f)
	out, err := hook(context.Background(), x.src.Agents[testAgentID])
	if err != nil || out.Phase != "" {
		t.Fatalf("done: %+v, %v", out, err)
	}
	y := newAgentFixture(deletingAgent(OnDeleteDelete, agent.IdentityTeamShared))
	y.work.stopped = false
	out, err = AgentDeleting(y.f)(context.Background(), y.src.Agents[testAgentID])
	if err != nil || out.Phase != "Deleting" || out.Requeue == 0 {
		t.Fatalf("pending: %+v, %v", out, err)
	}
	z := newAgentFixture(deletingAgent(OnDeleteDelete, agent.IdentityTeamShared))
	z.work.stopErr = errors.New("boom")
	if _, err = AgentDeleting(z.f)(context.Background(), z.src.Agents[testAgentID]); err == nil {
		t.Fatal("want error")
	}
}

func TestTeamAgentsStep(t *testing.T) {
	ctx := context.Background()
	x := newAgentFixture(deletingAgent(OnDeleteDelete, agent.IdentityTeamShared))
	step := TeamAgentsStep(x.src, x.f)
	tgt := Target{Kind: "team", ID: x.src.Agents[testAgentID].Team.ID, TeamID: x.src.Agents[testAgentID].Team.ID, Slug: "core"}

	x.work.stopped = false
	if err := step.Run(ctx, tgt); !errors.Is(err, ErrPending) {
		t.Fatalf("err = %v, want pending", err)
	}
	x.work.stopped = true
	// fakeStore returns the first agent's conditions for all; one agent is enough here.
	if err := step.Run(ctx, tgt); err != nil {
		t.Fatalf("err = %v", err)
	}
	if x.comp.n != 1 {
		t.Errorf("agent completions = %d", x.comp.n)
	}
	x.src.Err = errors.New("boom")
	if err := step.Run(ctx, tgt); err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("list error = %v", err)
	}
	x.src.Err = nil
	x.work.stopErr = errors.New("stop failed")
	y := newAgentFixture(deletingAgent(OnDeleteDelete, agent.IdentityTeamShared))
	y.work.stopErr = errors.New("stop failed")
	if err := TeamAgentsStep(y.src, y.f).Run(ctx, tgt); err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("agent error = %v", err)
	}
}
