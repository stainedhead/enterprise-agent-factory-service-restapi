package agent_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent/agenttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

const (
	teamID  = "team_01JD0000000000000000000011"
	agentID = "agt_01JD0000000000000000000021"
)

func desired() agent.Desired {
	return agent.Desired{
		ID: agentID, Name: "reviewer", Template: "hermes", Image: "registry.example.com/hermes@sha256:abc", ModelID: "m1",
		State: agent.StateEnabled, Generation: 3, IAMIdentity: agent.IdentityTeamShared,
		Volume: agent.Volume{SizeGiB: 20, StorageClass: "gp3", OnDelete: "snapshot_then_delete", SnapshotSchedule: "daily", SnapshotRetain: 7},
		Team:   agent.Team{ID: teamID, Slug: "alpha", Namespace: "afs-t-alpha", State: "active", IsolationTier: agent.TierShared},
		Tags:   map[string]string{"afs-team-id": teamID, "afs-agent-id": agentID},
	}
}

type rig struct {
	src  *agenttest.Source
	cl   *agenttest.Cluster
	iam  *teamtest.IAM
	pi   *agenttest.PodIdentities
	vols *agenttest.Volumes
	snap *agenttest.Snapshots
	ctl  *agent.Controller
}

func newRig(t *testing.T, d agent.Desired, hooks agent.Hooks) *rig {
	t.Helper()
	r := &rig{src: agenttest.NewSource(d), cl: &agenttest.Cluster{}, iam: teamtest.NewIAM(), pi: agenttest.NewPodIdentities(),
		vols: agenttest.NewVolumes(), snap: &agenttest.Snapshots{Latest: map[string]string{}}}
	r.vols.States[d.ID] = agent.VolumeState{Exists: true, Bound: true, RequestedGiB: d.Volume.SizeGiB, CapacityGiB: d.Volume.SizeGiB}
	ctl, err := agent.NewController(agent.Config{
		Source: r.src, Cluster: r.cl, IAM: r.iam, PodIdentity: r.pi, Volumes: r.vols, Snapshots: r.snap, Hooks: hooks,
		AWS: awsCfg(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.ctl = ctl
	return r
}

func ref() engine.Ref { return engine.NewRef(engine.KindAgent, agentID, teamID) }

func cond(out engine.Outcome, typ string) (domain.Condition, bool) {
	for _, c := range out.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return domain.Condition{}, false
}

func TestNewControllerRequiresPorts(t *testing.T) {
	if _, err := agent.NewController(agent.Config{}); err == nil {
		t.Error("empty config accepted")
	}
	if _, err := agent.NewController(agent.Config{Source: agenttest.NewSource(), Cluster: &agenttest.Cluster{}, IAM: teamtest.NewIAM(), PodIdentity: agenttest.NewPodIdentities()}); err == nil {
		t.Error("config without AWS settings accepted")
	}
}

// M2-F1: a reconcile applies the workload with replicas 1, the pod-template hash and the team role.
func TestReconcileAppliesWorkload(t *testing.T) {
	r := newRig(t, desired(), agent.Hooks{})
	out, err := r.ctl.Reconcile(t.Context(), ref())
	if err != nil {
		t.Fatal(err)
	}
	env := r.cl.Last()
	if env.Replicas != 1 || env.Hash == "" || env.Agent.ID != agentID {
		t.Errorf("environment = %+v", env)
	}
	wantRole := "arn:aws:iam::111122223333:role/afs/afs-t-alpha/afs-01JD0000000000000000000011"
	if env.RoleARN != wantRole || r.pi.Assoc["afs-t-alpha/agent-reviewer"] != wantRole {
		t.Errorf("role = %q, association = %v; want %s", env.RoleARN, r.pi.Assoc, wantRole)
	}
	if out.Generation != 3 || out.Phase != domain.PhaseReady {
		t.Errorf("outcome = %+v", out)
	}
	for _, typ := range []string{"IamReady", "SecretsSynced", "VolumeReady", "TemplateRetired", "Ready"} {
		if _, ok := cond(out, typ); !ok {
			t.Errorf("condition %s missing: %+v", typ, out.Conditions)
		}
	}
	if len(r.iam.Roles) != 0 {
		t.Errorf("a team-shared agent created roles: %v", r.iam.Roles)
	}
}

func TestReplicas(t *testing.T) {
	tests := []struct {
		name  string
		state string
		team  string
		want  int32
	}{
		{"enabled", agent.StateEnabled, "active", 1},
		{"paused", agent.StatePaused, "active", 0},
		{"team suspended", agent.StateEnabled, "suspended", 0},
		{"paused and suspended", agent.StatePaused, "suspended", 0},
	}
	for _, tt := range tests {
		d := desired()
		d.State, d.Team.State = tt.state, tt.team
		r := newRig(t, d, agent.Hooks{})
		if _, err := r.ctl.Reconcile(t.Context(), ref()); err != nil {
			t.Fatal(tt.name, err)
		}
		if got := r.cl.Last().Replicas; got != tt.want {
			t.Errorf("%s: replicas = %d, want %d", tt.name, got, tt.want)
		}
	}
}

// M2-F7: a dedicated agent gets its own role with the team and agent tags; a shared one uses the team role.
func TestDedicatedIdentity(t *testing.T) {
	d := desired()
	d.IAMIdentity = agent.IdentityDedicated
	r := newRig(t, d, agent.Hooks{})
	if _, err := r.ctl.Reconcile(t.Context(), ref()); err != nil {
		t.Fatal(err)
	}
	spec, ok := r.iam.Role("afs-01JD0000000000000000000021")
	if !ok {
		t.Fatalf("dedicated role not created: %v", r.iam.Calls)
	}
	if spec.Tags["afs-team-id"] != teamID || spec.Tags["afs-agent-id"] != agentID {
		t.Errorf("role tags = %v", spec.Tags)
	}
	if spec.Path != "/afs/afs-t-alpha/" || spec.PermissionsBoundaryARN == "" {
		t.Errorf("role path/boundary = %q %q", spec.Path, spec.PermissionsBoundaryARN)
	}
	if strings.Contains(spec.Policies["agent"], `"bedrock:*"`) && !strings.Contains(spec.Policies["agent"], `"Deny"`) {
		t.Error("dedicated role may use bedrock")
	}
	want := "arn:aws:iam::111122223333:role/afs/afs-t-alpha/afs-01JD0000000000000000000021"
	if got := r.pi.Assoc["afs-t-alpha/agent-reviewer"]; got != want {
		t.Errorf("association = %q, want %q", got, want)
	}
	if got := r.pi.Tags["afs-t-alpha/agent-reviewer"]["afs-agent-id"]; got != agentID {
		t.Errorf("association tags = %v", r.pi.Tags)
	}
}

// IAM and the cluster are independent: a failing role does not hide the workload apply, and the error
// names the condition.
func TestIAMFailureKeepsClusterApply(t *testing.T) {
	d := desired()
	d.IAMIdentity = agent.IdentityDedicated
	r := newRig(t, d, agent.Hooks{})
	r.iam.Err = errors.New("AccessDenied")
	out, err := r.ctl.Reconcile(t.Context(), ref())
	var ce *engine.ConditionError
	if !errors.As(err, &ce) || ce.Type != "IamReady" {
		t.Fatalf("error = %v, want IamReady ConditionError", err)
	}
	if len(r.cl.Envs) != 1 {
		t.Error("the cluster apply was skipped")
	}
	if c, ok := cond(out, "SecretsSynced"); !ok || c.Status != domain.ConditionTrue {
		t.Errorf("SecretsSynced missing from partial outcome: %+v", out.Conditions)
	}
}

func TestApplyFailureNamesSecretsSynced(t *testing.T) {
	r := newRig(t, desired(), agent.Hooks{})
	r.cl.ApplyErr = errors.New("forbidden")
	_, err := r.ctl.Reconcile(t.Context(), ref())
	var ce *engine.ConditionError
	if !errors.As(err, &ce) || ce.Type != "SecretsSynced" {
		t.Fatalf("error = %v", err)
	}
}

func TestPodIdentityFailureIsIamReady(t *testing.T) {
	r := newRig(t, desired(), agent.Hooks{})
	r.pi.Err = errors.New("throttled")
	_, err := r.ctl.Reconcile(t.Context(), ref())
	var ce *engine.ConditionError
	if !errors.As(err, &ce) || ce.Type != "IamReady" {
		t.Fatalf("error = %v", err)
	}
}

// M2-F6: TemplateRetired is True while the template is retired, the agent keeps being applied, and it is
// False again afterwards.
func TestTemplateRetired(t *testing.T) {
	d := desired()
	d.TemplateRetired, d.TemplateReason = true, "template hermes is disabled"
	r := newRig(t, d, agent.Hooks{})
	out, err := r.ctl.Reconcile(t.Context(), ref())
	if err != nil {
		t.Fatal(err)
	}
	c, _ := cond(out, "TemplateRetired")
	if c.Status != domain.ConditionTrue || !strings.Contains(c.Message, "disabled") {
		t.Errorf("TemplateRetired = %+v", c)
	}
	if out.Phase != domain.PhaseReady || r.cl.Last().Replicas != 1 {
		t.Errorf("a retired template must not stop the agent: phase %s replicas %d", out.Phase, r.cl.Last().Replicas)
	}
	d.TemplateRetired = false
	r.src.Put(d)
	out, _ = r.ctl.Reconcile(t.Context(), ref())
	if c, _ := cond(out, "TemplateRetired"); c.Status != domain.ConditionFalse {
		t.Errorf("TemplateRetired after re-enable = %+v", c)
	}
}

func TestGoneAndDeletedAgents(t *testing.T) {
	r := newRig(t, desired(), agent.Hooks{})
	out, err := r.ctl.Reconcile(t.Context(), engine.NewRef(engine.KindAgent, "agt_01JD0000000000000000000099", teamID))
	if err != nil || !out.NoStatus {
		t.Errorf("missing agent: %+v, %v", out, err)
	}
	d := desired()
	d.State = agent.StateDeleted
	r.src.Put(d)
	out, err = r.ctl.Reconcile(t.Context(), ref())
	if err != nil || !out.NoStatus || len(r.cl.Envs) != 0 {
		t.Errorf("deleted agent: %+v, %v, applies %d", out, err, len(r.cl.Envs))
	}
	r.src.Err = errors.New("db down")
	if _, err := r.ctl.Reconcile(t.Context(), ref()); err == nil {
		t.Error("source error swallowed")
	}
}

func TestDeletingUsesHook(t *testing.T) {
	d := desired()
	d.State = agent.StateDeleting
	called := 0
	r := newRig(t, d, agent.Hooks{Deleting: func(_ context_, got agent.Desired) (engine.Outcome, error) {
		called++
		return engine.Outcome{Phase: domain.PhaseDeleting, NoStatus: true}, nil
	}})
	if _, err := r.ctl.Reconcile(t.Context(), ref()); err != nil || called != 1 || len(r.cl.Envs) != 0 {
		t.Errorf("hook calls %d, applies %d, err %v", called, len(r.cl.Envs), err)
	}
	r2 := newRig(t, d, agent.Hooks{})
	out, _ := r2.ctl.Reconcile(t.Context(), ref())
	if out.Phase != domain.PhaseDeleting || !out.NoStatus {
		t.Errorf("without a hook: %+v", out)
	}
}

// A deleting team owns the teardown of its agents; the agent controller must not re-create anything.
func TestDeletingTeamIsNotApplied(t *testing.T) {
	d := desired()
	d.Team.State = "deleting"
	r := newRig(t, d, agent.Hooks{})
	out, err := r.ctl.Reconcile(t.Context(), ref())
	if err != nil || len(r.cl.Envs) != 0 || out.Phase != domain.PhaseDeleting {
		t.Errorf("out %+v err %v applies %d", out, err, len(r.cl.Envs))
	}
}

// M2-G1, G5: volume expansion and restore from the latest snapshot.
func TestVolumeReconcile(t *testing.T) {
	tests := []struct {
		name       string
		state      agent.VolumeState
		snapshots  map[string]string
		wantStatus domain.ConditionStatus
		wantReason string
		wantExpand bool
		wantRest   string
		wantPhase  domain.Phase
	}{
		{"bound", agent.VolumeState{Exists: true, Bound: true, RequestedGiB: 20, CapacityGiB: 20}, nil, domain.ConditionTrue, "Bound", false, "", domain.PhaseReady},
		{"grown", agent.VolumeState{Exists: true, Bound: true, RequestedGiB: 10, CapacityGiB: 10}, nil, domain.ConditionFalse, "Resizing", true, "", domain.PhaseReconciling},
		{"resizing", agent.VolumeState{Exists: true, Bound: true, RequestedGiB: 20, CapacityGiB: 10, Resizing: true}, nil, domain.ConditionFalse, "Resizing", false, "", domain.PhaseReconciling},
		{"unbound", agent.VolumeState{Exists: true, RequestedGiB: 20}, nil, domain.ConditionFalse, "Binding", false, "", domain.PhaseReconciling},
		{"new agent", agent.VolumeState{}, nil, domain.ConditionUnknown, "WaitingForPod", false, "", domain.PhaseReconciling},
		{"missing with snapshot", agent.VolumeState{}, map[string]string{agentID: "snap-0123456789abcdef0"}, domain.ConditionFalse, "Restoring", false, "snap-0123456789abcdef0", domain.PhaseReconciling},
	}
	for _, tt := range tests {
		r := newRig(t, desired(), agent.Hooks{})
		r.vols.States[agentID] = tt.state
		r.snap.Latest = tt.snapshots
		out, err := r.ctl.Reconcile(t.Context(), ref())
		if err != nil {
			t.Fatal(tt.name, err)
		}
		c, _ := cond(out, "VolumeReady")
		if c.Status != tt.wantStatus || c.Reason != tt.wantReason || out.Phase != tt.wantPhase {
			t.Errorf("%s: VolumeReady %+v phase %s", tt.name, c, out.Phase)
		}
		if got := len(r.vols.Expanded) == 1; got != tt.wantExpand {
			t.Errorf("%s: expanded = %v", tt.name, r.vols.Expanded)
		}
		if r.vols.Restored[agentID] != tt.wantRest {
			t.Errorf("%s: restored from %q, want %q", tt.name, r.vols.Restored[agentID], tt.wantRest)
		}
		if tt.wantPhase == domain.PhaseReconciling && out.Requeue != agent.VolumePollInterval {
			t.Errorf("%s: requeue = %v", tt.name, out.Requeue)
		}
	}
}

func TestVolumeErrorsNameVolumeReady(t *testing.T) {
	r := newRig(t, desired(), agent.Hooks{})
	r.vols.Err = errors.New("forbidden")
	_, err := r.ctl.Reconcile(t.Context(), ref())
	var ce *engine.ConditionError
	if !errors.As(err, &ce) || ce.Type != "VolumeReady" {
		t.Fatalf("error = %v", err)
	}
	r = newRig(t, desired(), agent.Hooks{})
	r.vols.States[agentID] = agent.VolumeState{}
	r.snap.Err = errors.New("db")
	if _, err := r.ctl.Reconcile(t.Context(), ref()); err == nil {
		t.Error("snapshot lookup error swallowed")
	}
}
