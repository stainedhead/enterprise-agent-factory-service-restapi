package pool_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent/agenttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool/pooltest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

const (
	teamID  = "team_01JD0000000000000000000011"
	poolA   = "pool_01JD0000000000000000000031"
	poolB   = "pool_01JD0000000000000000000032"
	poolC   = "pool_01JD0000000000000000000033"
	ns      = "afs-t-swarm"
	account = "111122223333"
)

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func desired(id, name string) pool.Desired {
	return pool.Desired{
		ID: id, Name: name, Template: "hermes", Image: "registry.example.com/hermes@sha256:abc", ModelID: "m1",
		State: pool.StateEnabled, Generation: 4, Workers: 5, ScratchGiB: 20, SharedVolume: pool.SharedNone,
		Team: agent.Team{ID: teamID, Slug: "swarm", Namespace: ns, State: "active", IsolationTier: agent.TierShared},
		Tags: map[string]string{"cost-center": "cc-1"},
	}
}

type rig struct {
	src  *pooltest.Source
	cl   *pooltest.Cluster
	iam  *teamtest.IAM
	pi   *agenttest.PodIdentities
	q    *pooltest.Queues
	rec  *pooltest.Recorder
	prb  *pooltest.Probe
	ctl  *pool.Controller
	pods []pool.Desired
}

func awsCfg() team.AWSConfig {
	return team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: account,
		GatewayRoleARN: "arn:aws:iam::" + account + ":role/afs-gateway", PermissionsBoundaryARN: "arn:aws:iam::" + account + ":policy/afs-boundary", SecretsPrefix: "afs"}
}

func newRig(t *testing.T, hooks pool.Hooks, pools ...pool.Desired) *rig {
	t.Helper()
	r := &rig{src: pooltest.NewSource(pools...), cl: &pooltest.Cluster{State: pool.Workers{Exists: true, Desired: 5, Ready: 5}},
		iam: teamtest.NewIAM(), pi: agenttest.NewPodIdentities(), q: pooltest.NewQueues(), rec: &pooltest.Recorder{}, prb: &pooltest.Probe{Fail: map[string]error{}}}
	ctl, err := pool.NewController(pool.Config{
		Source: r.src, Cluster: r.cl, IAM: r.iam, PodIdentity: r.pi, Queues: r.q, Probe: r.prb, Recorder: r.rec,
		AWS: awsCfg(), Hooks: hooks, LinkVerifierARN: "arn:aws:iam::" + account + ":role/afs-reconciler", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.ctl = ctl
	return r
}

func ref(id string) engine.Ref { return engine.NewRef(engine.KindPool, id, teamID) }

func cond(out engine.Outcome, typ string) (domain.Condition, bool) {
	for _, c := range out.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return domain.Condition{}, false
}

func roleARN(id string) string {
	_, ulid, _ := strings.Cut(id, "_")
	return "arn:aws:iam::" + account + ":role/afs/" + ns + "/afs-" + ulid
}

func TestNewControllerRequiresPorts(t *testing.T) {
	if _, err := pool.NewController(pool.Config{}); err == nil {
		t.Error("empty config accepted")
	}
	base := pool.Config{Source: pooltest.NewSource(), Cluster: &pooltest.Cluster{}, IAM: teamtest.NewIAM(), PodIdentity: agenttest.NewPodIdentities()}
	if _, err := pool.NewController(base); err == nil {
		t.Error("config without queues accepted")
	}
	base.Queues = pooltest.NewQueues()
	if _, err := pool.NewController(base); err == nil {
		t.Error("config without AWS settings accepted")
	}
	base.AWS = awsCfg()
	if _, err := pool.NewController(base); err != nil {
		t.Errorf("complete config refused: %v", err)
	}
}

// M5-C1: a reconcile applies one workload with `workers` replicas, the pool's own role and Pod Identity
// association, and reports the conditions.
func TestReconcileAppliesWorkload(t *testing.T) {
	r := newRig(t, pool.Hooks{}, desired(poolA, "fixer"))
	out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
	if err != nil {
		t.Fatal(err)
	}
	env := r.cl.Last()
	if env.Replicas != 5 || env.Hash == "" || env.Pool.ID != poolA {
		t.Errorf("environment = %+v", env)
	}
	if env.RoleARN != roleARN(poolA) || r.pi.Assoc[ns+"/pool-fixer"] != roleARN(poolA) {
		t.Errorf("role = %q, association = %v; want %s", env.RoleARN, r.pi.Assoc, roleARN(poolA))
	}
	if out.Generation != 4 || out.Phase != domain.PhaseReady {
		t.Errorf("outcome = %+v", out)
	}
	for _, typ := range []string{"IamReady", "TagsApplied", "SecretsSynced", "EgressPolicyReady", "WorkersReady", "TemplateRetired", "Ready"} {
		if _, ok := cond(out, typ); !ok {
			t.Errorf("condition %s missing: %+v", typ, out.Conditions)
		}
	}
	spec, ok := r.iam.Role("afs-01JD0000000000000000000031")
	if !ok {
		t.Fatalf("pool role not created: %v", r.iam.Calls)
	}
	if spec.Tags["afs-pool-id"] != poolA || spec.Tags["afs-team-id"] != teamID || spec.Tags["afs-pool-name"] != "fixer" || spec.Tags["cost-center"] != "cc-1" {
		t.Errorf("role tags = %v", spec.Tags)
	}
	if !strings.Contains(spec.TrustPolicy, "afs-reconciler") {
		t.Errorf("trust policy does not name the link verifier: %s", spec.TrustPolicy)
	}
	if _, queued := cond(out, pool.CondInboxReady); queued || len(r.q.Names()) != 0 {
		t.Errorf("a pool that receives nothing got an inbox: %v", r.q.Names())
	}
}

// M5-C1: scaling changes replicas only; the pod-template hash does not change with the count or the state.
func TestScalingChangesReplicasOnly(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*pool.Desired)
		replicas int32
	}{
		{"enabled", func(*pool.Desired) {}, 5},
		{"scaled to 0", func(d *pool.Desired) { d.Workers = 0 }, 0},
		{"scaled to 200", func(d *pool.Desired) { d.Workers = 200 }, 200},
		{"paused", func(d *pool.Desired) { d.State = pool.StatePaused }, 0},
		{"team suspended", func(d *pool.Desired) { d.Team.State = "suspended" }, 0},
		{"paused and suspended", func(d *pool.Desired) { d.State, d.Team.State = pool.StatePaused, "suspended" }, 0},
	}
	base := newRig(t, pool.Hooks{}, desired(poolA, "fixer"))
	if _, err := base.ctl.Reconcile(t.Context(), ref(poolA)); err != nil {
		t.Fatal(err)
	}
	baseHash := base.cl.Last().Hash
	for _, tt := range tests {
		d := desired(poolA, "fixer")
		tt.mutate(&d)
		r := newRig(t, pool.Hooks{}, d)
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil {
			t.Fatal(tt.name, err)
		}
		if got := r.cl.Last(); got.Replicas != tt.replicas || got.Hash != baseHash {
			t.Errorf("%s: replicas = %d (want %d), hash %s (want unchanged %s)", tt.name, got.Replicas, tt.replicas, got.Hash, baseHash)
		}
	}
}

// M5-C4: a resume restores what the suspend took: the row's worker count, and a pool paused before the
// suspend stays paused.
func TestResumeRestoresPriorState(t *testing.T) {
	d := desired(poolA, "fixer")
	r := newRig(t, pool.Hooks{}, d)
	step := func(mutate func(*pool.Desired)) int32 {
		t.Helper()
		cur := d
		mutate(&cur)
		r.src.Put(cur)
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil {
			t.Fatal(err)
		}
		return r.cl.Last().Replicas
	}
	if got := step(func(*pool.Desired) {}); got != 5 {
		t.Fatalf("running = %d", got)
	}
	if got := step(func(c *pool.Desired) { c.Team.State = "suspended" }); got != 0 {
		t.Fatalf("suspended = %d", got)
	}
	if got := step(func(*pool.Desired) {}); got != 5 {
		t.Fatalf("resumed = %d, want 5", got)
	}
	if got := step(func(c *pool.Desired) { c.State = pool.StatePaused }); got != 0 {
		t.Fatalf("paused = %d", got)
	}
	if got := step(func(c *pool.Desired) { c.State = pool.StatePaused; c.Team.State = "suspended" }); got != 0 {
		t.Fatalf("paused then suspended = %d", got)
	}
	if got := step(func(c *pool.Desired) { c.State = pool.StatePaused }); got != 0 {
		t.Fatalf("team resumed, pool still paused = %d, want 0", got)
	}
	if got := step(func(*pool.Desired) {}); got != 5 {
		t.Fatalf("pool resumed = %d", got)
	}
}

func TestPodTemplateHash(t *testing.T) {
	base := desired(poolA, "fixer")
	base.Bindings = []agent.Binding{{SecretID: "sec_1", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/a-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "TOKEN"}} //nolint:gosec // an example ARN
	base.Config = []agent.ConfigValue{{Name: "LOG", Value: "info"}}
	h0 := pool.PodTemplateHash(base, "", nil)
	tests := []struct {
		name    string
		mutate  func(*pool.Desired)
		changes bool
	}{
		{"workers", func(d *pool.Desired) { d.Workers = 99 }, false},
		{"paused", func(d *pool.Desired) { d.State = pool.StatePaused }, false},
		{"team suspended", func(d *pool.Desired) { d.Team.State = "suspended" }, false},
		{"generation", func(d *pool.Desired) { d.Generation++ }, false},
		{"image", func(d *pool.Desired) { d.Image += "x" }, true},
		{"persona", func(d *pool.Desired) { d.Persona = "p" }, true},
		{"secret version", func(d *pool.Desired) { d.Bindings[0].VersionID = "v2" }, true},
		{"config value", func(d *pool.Desired) { d.Config[0].Value = "debug" }, true},
		{"scratch", func(d *pool.Desired) { d.ScratchGiB = 21 }, true},
		{"shared volume", func(d *pool.Desired) { d.SharedVolume = pool.SharedEFS }, true},
		{"switched-off secret", func(d *pool.Desired) { d.Bindings[0].ExternalSystemID, d.Bindings[0].SystemOff = "ext_1", true }, true},
	}
	for _, tt := range tests {
		d := base
		d.Bindings = append([]agent.Binding(nil), base.Bindings...)
		d.Config = append([]agent.ConfigValue(nil), base.Config...)
		tt.mutate(&d)
		if got := pool.PodTemplateHash(d, "", nil) != h0; got != tt.changes {
			t.Errorf("%s: hash changed = %v, want %v", tt.name, got, tt.changes)
		}
	}
	if pool.PodTemplateHash(base, "https://q/1", nil) == h0 || pool.PodTemplateHash(base, "", map[string]string{"b": "https://q/2"}) == h0 {
		t.Error("queue urls are not part of the hash")
	}
}

// M5-C2: a receiving pool gets one inbox queue that only its senders may send to and only it may receive
// from; a sending pool gets the receiver's URL and the right to send to that queue and no other.
func TestHandoffQueues(t *testing.T) {
	a, b, c := desired(poolA, "fixer"), desired(poolB, "reviewer"), desired(poolC, "scanner")
	// scanner -> fixer, scanner -> reviewer, fixer -> reviewer
	a.SendsTo, a.ReceivesFrom = []pool.Peer{{ID: poolB, Name: "reviewer"}}, []pool.Peer{{ID: poolC, Name: "scanner"}}
	b.ReceivesFrom = []pool.Peer{{ID: poolA, Name: "fixer"}, {ID: poolC, Name: "scanner"}}
	c.SendsTo = []pool.Peer{{ID: poolA, Name: "fixer"}, {ID: poolB, Name: "reviewer"}}
	r := newRig(t, pool.Hooks{}, a, b, c)
	outs := map[string]engine.Outcome{}
	for _, id := range []string{poolA, poolB, poolC} {
		out, err := r.ctl.Reconcile(t.Context(), ref(id))
		if err != nil {
			t.Fatal(id, err)
		}
		outs[id] = out
	}

	nameRE := regexp.MustCompile(`^afs-01JD0000000000000000000011-[0-9a-f]{8}$`)
	names := r.q.Names()
	if len(names) != 2 {
		t.Fatalf("queues = %v, want one inbox for each of the two receiving pools", names)
	}
	for _, n := range names {
		if !nameRE.MatchString(n) {
			t.Errorf("queue name %q does not match afs-<team ulid>-<short hash>", n)
		}
	}
	qa, _ := pool.QueueName(teamID, poolA)
	qb, _ := pool.QueueName(teamID, poolB)
	if qa == qb {
		t.Fatal("two pools share a queue name")
	}

	inboxB := r.q.Live[qb]
	if inboxB.ReceiverRoleARN != roleARN(poolB) {
		t.Errorf("receiver of reviewer's inbox = %s", inboxB.ReceiverRoleARN)
	}
	if got, want := strings.Join(inboxB.SenderRoleARNs, ","), roleARN(poolA)+","+roleARN(poolC); got != want {
		t.Errorf("senders of reviewer's inbox = %s, want %s (the pools that hand off to it, and nobody else)", got, want)
	}
	if got := r.q.Live[qa].SenderRoleARNs; len(got) != 1 || got[0] != roleARN(poolC) {
		t.Errorf("senders of fixer's inbox = %v", got)
	}

	if c := r.cl.Envs; len(c) != 3 {
		t.Fatalf("applies = %d", len(c))
	}
	envA, envB, envC := r.cl.Envs[0], r.cl.Envs[1], r.cl.Envs[2]
	if envA.InboxURL != r.q.Locate(qa).URL || envB.InboxURL != r.q.Locate(qb).URL || envC.InboxURL != "" {
		t.Errorf("inbox urls: fixer %q reviewer %q scanner %q", envA.InboxURL, envB.InboxURL, envC.InboxURL)
	}
	if len(envC.HandoffURLs) != 2 || envC.HandoffURLs["fixer"] != r.q.Locate(qa).URL || envC.HandoffURLs["reviewer"] != r.q.Locate(qb).URL {
		t.Errorf("scanner handoff urls = %v", envC.HandoffURLs)
	}
	if len(envB.HandoffURLs) != 0 {
		t.Errorf("reviewer sends to nobody but got %v", envB.HandoffURLs)
	}
	if _, ok := cond(outs[poolB], pool.CondInboxReady); !ok {
		t.Error("InboxReady missing for the receiving pool")
	}
	if _, ok := cond(outs[poolC], pool.CondInboxReady); ok {
		t.Error("InboxReady reported for a pool with no inbox")
	}

	// IAM: a sender may send to exactly the queues it hands to; the receiver may receive from its own.
	assertStatements := func(id string, wantSendTo []string, wantReceive string) {
		t.Helper()
		_, ulid, _ := strings.Cut(id, "_")
		spec, ok := r.iam.Role("afs-" + ulid)
		if !ok {
			t.Fatalf("role of %s missing", id)
		}
		var doc struct {
			Statement []struct {
				Sid, Effect      string
				Action, Resource []string
			}
		}
		if err := json.Unmarshal([]byte(spec.Policies["pool"]), &doc); err != nil {
			t.Fatal(err)
		}
		var send, recv []string
		for _, s := range doc.Statement {
			switch s.Sid {
			case pool.SidSendHandoffs:
				send = s.Resource
			case pool.SidReceiveInbox:
				recv = s.Resource
			}
			for _, res := range s.Resource {
				if strings.Contains(res, "sqs") && strings.Contains(res, "*") {
					t.Errorf("%s: wildcard queue resource %s", id, res)
				}
			}
		}
		wantARNs := make([]string, len(wantSendTo))
		for i, n := range wantSendTo {
			wantARNs[i] = r.q.Locate(n).ARN
		}
		if strings.Join(send, ",") != strings.Join(wantARNs, ",") {
			t.Errorf("%s may send to %v, want %v", id, send, wantARNs)
		}
		if (wantReceive == "") != (len(recv) == 0) || (wantReceive != "" && recv[0] != r.q.Locate(wantReceive).ARN) {
			t.Errorf("%s may receive from %v, want %q", id, recv, wantReceive)
		}
	}
	sorted := func(x, y string) []string {
		if x < y {
			return []string{x, y}
		}
		return []string{y, x}
	}
	assertStatements(poolA, []string{qb}, qa)
	assertStatements(poolB, nil, qb)
	assertStatements(poolC, sorted(qa, qb), "")
}

// A failed queue does not stop the workload: the pods get the deterministic URL and the condition says why.
func TestQueueFailureIsReportedAndDoesNotBlockWorkload(t *testing.T) {
	b := desired(poolB, "reviewer")
	b.ReceivesFrom = []pool.Peer{{ID: poolA, Name: "fixer"}}
	r := newRig(t, pool.Hooks{}, b)
	r.q.EnsureErr = errors.New("queue boom")
	out, err := r.ctl.Reconcile(t.Context(), ref(poolB))
	if err == nil {
		t.Fatal("no error")
	}
	var ce *engine.ConditionError
	if !errors.As(err, &ce) || ce.Type != pool.CondInboxReady {
		t.Errorf("error = %v, want a failure of %s", err, pool.CondInboxReady)
	}
	qb, _ := pool.QueueName(teamID, poolB)
	if got := r.cl.Last(); got.InboxURL != r.q.Locate(qb).URL {
		t.Errorf("inbox url = %q", got.InboxURL)
	}
	if _, ok := cond(out, "SecretsSynced"); !ok {
		t.Error("the workload was not reconciled")
	}
}

func TestIdentityFailures(t *testing.T) {
	t.Run("role", func(t *testing.T) {
		r := newRig(t, pool.Hooks{}, desired(poolA, "fixer"))
		r.iam.Err = errors.New("iam boom")
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		var ce *engine.ConditionError
		if !errors.As(err, &ce) || ce.Type != "IamReady" || out.Phase != "" {
			t.Fatalf("out %+v err %v", out, err)
		}
		if len(r.pi.Assoc) != 0 {
			t.Error("pod identity associated without a role")
		}
	})
	t.Run("pod identity", func(t *testing.T) {
		r := newRig(t, pool.Hooks{}, desired(poolA, "fixer"))
		r.pi.Err = errors.New("eks boom")
		_, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		var ce *engine.ConditionError
		if !errors.As(err, &ce) || ce.Type != "IamReady" {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("apply", func(t *testing.T) {
		r := newRig(t, pool.Hooks{}, desired(poolA, "fixer"))
		r.cl.ApplyErr = errors.New("k8s boom")
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		var ce *engine.ConditionError
		if !errors.As(err, &ce) || ce.Type != "SecretsSynced" {
			t.Fatalf("err %v", err)
		}
		if _, ok := cond(out, "IamReady"); !ok {
			t.Error("the identity's condition was hidden by the apply failure")
		}
		if _, ok := cond(out, pool.CondWorkersReady); ok {
			t.Error("workers reported after a failed apply")
		}
	})
	t.Run("tag limit", func(t *testing.T) {
		d := desired(poolA, "fixer")
		d.Tags = map[string]string{}
		for i := 0; i < domain.MaxEffectiveTags; i++ {
			d.Tags["k"+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
		}
		r := newRig(t, pool.Hooks{}, d)
		_, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		var ce *engine.ConditionError
		if !errors.As(err, &ce) || ce.Type != "TagsApplied" {
			t.Fatalf("err %v", err)
		}
		if len(r.cl.Envs) != 0 || len(r.iam.Roles) != 0 {
			t.Error("something was applied above the tag cap")
		}
	})
}

func TestWorkersCondition(t *testing.T) {
	tests := []struct {
		name    string
		d       func(*pool.Desired)
		state   pool.Workers
		status  domain.ConditionStatus
		reason  string
		phase   domain.Phase
		requeue bool
	}{
		{"all ready", func(*pool.Desired) {}, pool.Workers{Exists: true, Desired: 5, Ready: 5}, domain.ConditionTrue, "AllReady", domain.PhaseReady, false},
		{"scaling up", func(*pool.Desired) {}, pool.Workers{Exists: true, Desired: 5, Ready: 2}, domain.ConditionFalse, "Scaling", domain.PhaseReconciling, true},
		{"no deployment yet", func(*pool.Desired) {}, pool.Workers{}, domain.ConditionUnknown, "WaitingForDeployment", domain.PhaseReconciling, true},
		{"paused", func(d *pool.Desired) { d.State = pool.StatePaused }, pool.Workers{Exists: true, Ready: 3}, domain.ConditionTrue, "Paused", domain.PhaseReady, false},
		{"suspended", func(d *pool.Desired) { d.Team.State = "suspended" }, pool.Workers{Exists: true}, domain.ConditionTrue, "TeamSuspended", domain.PhaseReady, false},
		{"zero workers", func(d *pool.Desired) { d.Workers = 0 }, pool.Workers{Exists: true}, domain.ConditionTrue, "ScaledToZero", domain.PhaseReady, false},
	}
	for _, tt := range tests {
		d := desired(poolA, "fixer")
		tt.d(&d)
		r := newRig(t, pool.Hooks{}, d)
		r.cl.State = tt.state
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		if err != nil {
			t.Fatal(tt.name, err)
		}
		c, _ := cond(out, pool.CondWorkersReady)
		if c.Status != tt.status || c.Reason != tt.reason || out.Phase != tt.phase || (out.Requeue > 0) != tt.requeue {
			t.Errorf("%s: condition %+v phase %s requeue %v", tt.name, c, out.Phase, out.Requeue)
		}
	}
	r := newRig(t, pool.Hooks{}, desired(poolA, "fixer"))
	r.cl.StateErr = errors.New("api boom")
	if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err == nil {
		t.Error("a failed Deployment read was swallowed")
	}
}

// M5-C1: the egress policy carries only the systems that are on for the pool.
func TestEgressPolicy(t *testing.T) {
	d := desired(poolA, "fixer")
	d.Systems = []egress.System{
		{ID: "ext_1", Slug: "github", Endpoints: []string{"api.github.com", "*.githubusercontent.com"}},
		{ID: "ext_2", Slug: "okta", Endpoints: []string{"example.okta.com"}, DisabledAt: egress.OffPool},
	}
	r := newRig(t, pool.Hooks{}, d)
	out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
	if err != nil {
		t.Fatal(err)
	}
	pol := r.cl.Last().Egress
	if pol.AgentID != poolA || len(pol.Rules) != 1 || pol.Rules[0].Slug != "github" || len(pol.Rules[0].Endpoints) != 2 {
		t.Errorf("policy = %+v", pol)
	}
	if c, _ := cond(out, "EgressPolicyReady"); c.Status != domain.ConditionTrue || !strings.Contains(c.Message, "github") || strings.Contains(c.Message, "okta") {
		t.Errorf("condition = %+v", c)
	}

	d.Systems = []egress.System{{ID: "ext_3", Slug: "bad", Endpoints: []string{"https://nope/x"}}}
	r = newRig(t, pool.Hooks{}, d)
	out, err = r.ctl.Reconcile(t.Context(), ref(poolA))
	if err == nil {
		t.Fatal("an endpoint that cannot be matched was accepted")
	}
	if c, _ := cond(out, "EgressPolicyReady"); c.Status != domain.ConditionFalse || c.Reason != "InvalidEndpoint" {
		t.Errorf("condition = %+v", c)
	}
	if len(r.cl.Last().Egress.Rules) != 0 {
		t.Error("a policy with an invalid endpoint allowed something")
	}
}

func TestTemplateRetired(t *testing.T) {
	d := desired(poolA, "fixer")
	d.TemplateRetired, d.TemplateReason = true, "agent template hermes is disabled"
	r := newRig(t, pool.Hooks{}, d)
	out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := cond(out, "TemplateRetired"); c.Status != domain.ConditionTrue || c.Message != d.TemplateReason {
		t.Errorf("condition = %+v", c)
	}
	if r.cl.Last().Replicas != 5 {
		t.Error("a pool with a retired template stopped")
	}
}

func TestLifecycleStates(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		r := newRig(t, pool.Hooks{})
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		if err != nil || !out.NoStatus {
			t.Errorf("out %+v err %v", out, err)
		}
	})
	t.Run("source error", func(t *testing.T) {
		r := newRig(t, pool.Hooks{}, desired(poolA, "fixer"))
		r.src.Err = pooltest.ErrBoom
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); !errors.Is(err, pooltest.ErrBoom) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		d := desired(poolA, "fixer")
		d.State = pool.StateDeleted
		r := newRig(t, pool.Hooks{}, d)
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		if err != nil || !out.NoStatus || len(r.cl.Envs) != 0 {
			t.Errorf("out %+v err %v applies %d", out, err, len(r.cl.Envs))
		}
	})
	t.Run("deleting without a hook", func(t *testing.T) {
		d := desired(poolA, "fixer")
		d.State = pool.StateDeleting
		r := newRig(t, pool.Hooks{}, d)
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		if err != nil || out.Phase != domain.PhaseDeleting || !out.NoStatus || len(r.cl.Envs) != 0 {
			t.Errorf("out %+v err %v", out, err)
		}
	})
	t.Run("deleting runs the hook", func(t *testing.T) {
		d := desired(poolA, "fixer")
		d.State = pool.StateDeleting
		called := 0
		r := newRig(t, pool.Hooks{Deleting: func(_ context.Context, got pool.Desired) (engine.Outcome, error) {
			called++
			return engine.Outcome{Generation: got.Generation, Phase: domain.PhaseDeleting, NoStatus: true}, nil
		}}, d)
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil || called != 1 || len(r.cl.Envs) != 0 {
			t.Errorf("called %d err %v applies %d", called, err, len(r.cl.Envs))
		}
	})
	t.Run("team deleting", func(t *testing.T) {
		d := desired(poolA, "fixer")
		d.Team.State = "deleting"
		r := newRig(t, pool.Hooks{}, d)
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		if err != nil || out.Phase != domain.PhaseDeleting || len(r.cl.Envs) != 0 || len(r.iam.Roles) != 0 {
			t.Errorf("out %+v err %v: the pool was applied while its team is deleted", out, err)
		}
	})
}

func TestKindAndRoutes(t *testing.T) {
	r := newRig(t, pool.Hooks{})
	if r.ctl.Kind() != engine.KindPool {
		t.Errorf("kind = %s", r.ctl.Kind())
	}
	reg, err := engine.NewRegistry(pool.Routes()...)
	if err != nil {
		t.Fatal(err)
	}
	fan := func(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
		var out []engine.Ref
		for _, k := range kinds {
			out = append(out, engine.NewRef(k, "pool_x", "team_other"), engine.NewRef(k, poolA, teamID), engine.NewRef(k, poolB, teamID))
		}
		return out, nil
	}
	tests := []struct {
		table, team, id string
		want            int
	}{
		{"agent_pools", teamID, poolA, 1},
		{"pool_handoffs", teamID, poolA, 2},
		{"pool_secret_bindings", teamID, poolA, 2},
		{"pool_role_links", teamID, poolA, 2},
		{"teams", teamID, teamID, 2},
		{"agent_templates", "", "tpl_1", 3},
		{"agents", teamID, "agt_1", 0},
	}
	for _, tt := range tests {
		got, err := reg.Resolve(t.Context(), engine.Change{Table: tt.table, TeamID: tt.team, ID: tt.id}, fan)
		if err != nil || len(got) != tt.want {
			t.Errorf("%s: %d refs, err %v; want %d", tt.table, len(got), err, tt.want)
		}
	}
}
