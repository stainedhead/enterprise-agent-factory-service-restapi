package team_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

const (
	idA = "team_01JD0000000000000000000001"
	idB = "team_01JD0000000000000000000002"
	gw  = "arn:aws:iam::111122223333:role/afs-gateway"
)

var sonnet = "anthropic.claude-sonnet-4-5-v1:0"

type rig struct {
	c        *team.Controller
	src      *teamtest.Source
	cluster  *teamtest.Cluster
	iam      *teamtest.IAM
	profiles *teamtest.Profiles
}

func desired(id, slug string, models ...string) team.Desired {
	d := team.Desired{ID: id, Slug: slug, Namespace: "afs-t-" + slug, DisplayName: slug, Kind: "persistent", State: "active", Generation: 3,
		Quotas: team.Quotas{CPU: "32", Memory: "128Gi", Pods: 100}}
	for _, m := range models {
		d.Models = append(d.Models, team.Model{ID: m})
	}
	return d
}

func newRig(t *testing.T, teams ...team.Desired) *rig {
	t.Helper()
	r := &rig{src: teamtest.NewSource(teams...), cluster: &teamtest.Cluster{}, iam: teamtest.NewIAM(), profiles: teamtest.NewProfiles()}
	c, err := team.NewController(team.Config{
		Source: r.src, Cluster: r.cluster, IAM: r.iam, Profiles: r.profiles,
		AWS: team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: gw,
			PermissionsBoundaryARN: "arn:aws:iam::111122223333:policy/afs-boundary", ClusterARN: "arn:aws:eks:us-east-1:111122223333:cluster/afs", SecretsPrefix: "afs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	return r
}

func ref(id string) engine.Ref { return engine.NewRef(engine.KindTeam, id, id) }

func condition(out engine.Outcome, typ string) (domain.Condition, bool) {
	for _, c := range out.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return domain.Condition{}, false
}

func TestNewControllerValidation(t *testing.T) {
	if _, err := team.NewController(team.Config{}); err == nil {
		t.Error("empty config accepted")
	}
	base := team.Config{Source: teamtest.NewSource(), Cluster: &teamtest.Cluster{}, IAM: teamtest.NewIAM(), Profiles: teamtest.NewProfiles()}
	if _, err := team.NewController(base); err == nil || !strings.Contains(err.Error(), "AccountID") {
		t.Errorf("missing AWS config: %v", err)
	}
	if (&team.Controller{}).Kind() != engine.KindTeam {
		t.Error("Kind")
	}
}

// M1-G1d, M1-G2b: a new team converges: profiles, both roles, the cluster environment, conditions.
func TestReconcileConvergesATeam(t *testing.T) {
	r := newRig(t, desired(idA, "payments", sonnet, "us.anthropic.claude-haiku-4-5-v1:0"))
	r.cluster.Changed = []string{"Namespace/afs-t-payments"}
	out, err := r.c.Reconcile(t.Context(), ref(idA))
	if err != nil {
		t.Fatal(err)
	}
	if out.Generation != 3 || out.NoStatus || out.Phase != "" {
		t.Errorf("outcome = %+v", out)
	}
	for _, typ := range []string{domain.CondNamespaceReady, domain.CondIamReady, "Ready"} {
		if c, ok := condition(out, typ); !ok || c.Status != domain.ConditionTrue {
			t.Errorf("condition %s = %+v", typ, c)
		}
	}
	if len(out.Changed) != 1 || out.Changed[0] != "Namespace/afs-t-payments" {
		t.Errorf("Changed = %v", out.Changed)
	}

	// Roles by id under /afs/<namespace>/, boundary and tags.
	teamRole, ok := r.iam.Role("afs-01JD0000000000000000000001")
	bedrock, ok2 := r.iam.Role("afs-01JD0000000000000000000001-bedrock")
	if !ok || !ok2 {
		t.Fatalf("roles = %v", r.iam.Roles)
	}
	for _, role := range []team.RoleSpec{teamRole, bedrock} {
		if role.Path != "/afs/afs-t-payments/" || role.PermissionsBoundaryARN != "arn:aws:iam::111122223333:policy/afs-boundary" ||
			role.Tags[team.TagTeamID] != idA || role.Tags[team.TagTeamSlug] != "payments" {
			t.Errorf("role = %+v", role)
		}
	}
	if !strings.Contains(bedrock.TrustPolicy, gw) || strings.Contains(bedrock.TrustPolicy, "pods.eks") {
		t.Errorf("bedrock trust = %s", bedrock.TrustPolicy)
	}
	if !strings.Contains(teamRole.TrustPolicy, "pods.eks.amazonaws.com") || strings.Contains(teamRole.TrustPolicy, gw) {
		t.Errorf("team trust = %s", teamRole.TrustPolicy)
	}

	// One profile per model, recorded, and the Bedrock role allows exactly those.
	if len(r.profiles.ByName) != 2 || len(r.src.ARNs) != 2 {
		t.Fatalf("profiles = %v, recorded = %v", r.profiles.ByName, r.src.ARNs)
	}
	var doc struct {
		Statement []struct {
			Effect   string
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(bedrock.Policies["bedrock"]), &doc); err != nil {
		t.Fatal(err)
	}
	var allowed []string
	for _, p := range r.profiles.ByName {
		allowed = append(allowed, p.ARN)
	}
	for _, a := range allowed {
		if !strings.Contains(bedrock.Policies["bedrock"], a) {
			t.Errorf("profile %s not granted", a)
		}
	}
	if !strings.Contains(bedrock.Policies["bedrock"], "foundation-model/anthropic.claude-haiku-4-5-v1:0") {
		t.Errorf("a geo prefixed model must grant the foundation model without the prefix: %s", bedrock.Policies["bedrock"])
	}

	// The cluster got the environment with the team role ARN (no IAM round trip needed) and the defaults.
	if len(r.cluster.Envs) != 1 {
		t.Fatal("cluster not applied")
	}
	env := r.cluster.Envs[0]
	if env.TeamRoleARN != "arn:aws:iam::111122223333:role/afs/afs-t-payments/afs-01JD0000000000000000000001" || env.Region != "us-east-1" ||
		env.GatewayNamespace != "afs-system" || env.GatewayName != "afs-gateway" || env.GatewayPort != 8443 || env.Team.Namespace != "afs-t-payments" {
		t.Errorf("environment = %+v", env)
	}
}

// M1-G2b: no role but the Bedrock role is ever allowed Bedrock; the team role denies it explicitly.
func TestOnlyTheBedrockRoleMayInvokeModels(t *testing.T) {
	r := newRig(t, desired(idA, "payments", sonnet))
	if _, err := r.c.Reconcile(t.Context(), ref(idA)); err != nil {
		t.Fatal(err)
	}
	for name, role := range r.iam.Roles {
		for pname, doc := range role.Policies {
			var p struct {
				Statement []struct {
					Effect string
					Action []string
				}
			}
			_ = json.Unmarshal([]byte(doc), &p)
			for _, s := range p.Statement {
				for _, a := range s.Action {
					wildcard := a == "*" || a == "bedrock:*"
					if wildcard && s.Effect == "Allow" {
						t.Errorf("role %s policy %s allows %s", name, pname, a)
					}
					if strings.HasPrefix(a, "bedrock:") && s.Effect == "Allow" && !strings.HasSuffix(name, "-bedrock") {
						t.Errorf("non-Bedrock role %s allows %s", name, a)
					}
				}
			}
		}
	}
	tr, _ := r.iam.Role("afs-01JD0000000000000000000001")
	if !strings.Contains(tr.Policies["team"], `"Effect":"Deny"`) || !strings.Contains(tr.Policies["team"], "bedrock:*") {
		t.Errorf("team role does not deny bedrock:*: %s", tr.Policies["team"])
	}
}

// M1-G2b: two teams' resources never share a role or a profile.
func TestTwoTeamsGetDistinctResources(t *testing.T) {
	r := newRig(t, desired(idA, "alpha", sonnet), desired(idB, "beta", sonnet))
	for _, id := range []string{idA, idB} {
		if _, err := r.c.Reconcile(t.Context(), ref(id)); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.iam.Roles) != 4 || len(r.profiles.ByName) != 2 {
		t.Errorf("roles = %d, profiles = %d", len(r.iam.Roles), len(r.profiles.ByName))
	}
	a, _ := r.iam.Role("afs-01JD0000000000000000000001-bedrock")
	b, _ := r.iam.Role("afs-01JD0000000000000000000002-bedrock")
	if a.Path == b.Path || a.Policies["bedrock"] == b.Policies["bedrock"] {
		t.Error("two teams' Bedrock roles are not distinct")
	}
}

func TestReconcileIsIdempotentAndPrunesRemovedModels(t *testing.T) {
	r := newRig(t, desired(idA, "payments", sonnet, "anthropic.claude-haiku-4-5-v1:0"))
	if _, err := r.c.Reconcile(t.Context(), ref(idA)); err != nil {
		t.Fatal(err)
	}
	writes := len(r.src.ARNs)
	if _, err := r.c.Reconcile(t.Context(), ref(idA)); err != nil {
		t.Fatal(err)
	}
	if len(r.profiles.ByName) != 2 || len(r.src.ARNs) != writes {
		t.Errorf("second pass changed things: %d profiles", len(r.profiles.ByName))
	}
	// A model is removed from the team: its profile is deleted and the policy shrinks.
	r.src.Put(desired(idA, "payments", sonnet))
	if _, err := r.c.Reconcile(t.Context(), ref(idA)); err != nil {
		t.Fatal(err)
	}
	if len(r.profiles.ByName) != 1 {
		t.Errorf("profiles = %v", r.profiles.ByName)
	}
	b, _ := r.iam.Role("afs-01JD0000000000000000000001-bedrock")
	if strings.Contains(b.Policies["bedrock"], "haiku") {
		t.Errorf("policy still mentions the removed model: %s", b.Policies["bedrock"])
	}
	// No models at all: invocations are denied.
	r.src.Put(desired(idA, "payments"))
	if _, err := r.c.Reconcile(t.Context(), ref(idA)); err != nil {
		t.Fatal(err)
	}
	b, _ = r.iam.Role("afs-01JD0000000000000000000001-bedrock")
	if !strings.Contains(b.Policies["bedrock"], "NoModelsEnabled") || len(r.profiles.ByName) != 0 {
		t.Errorf("no-model policy = %s, profiles = %v", b.Policies["bedrock"], r.profiles.ByName)
	}
}

// M1-F4/M1-G1d: an AWS failure keeps the namespace result, names IamReady and carries the AWS code.
func TestIAMFailureStillAppliesTheCluster(t *testing.T) {
	r := newRig(t, desired(idA, "payments", sonnet))
	r.iam.Err = &engine.CodedError{Code: "AccessDenied", Err: errors.New("not authorized to perform iam:CreateRole")}
	out, err := r.c.Reconcile(t.Context(), ref(idA))
	if err == nil || engine.CodeOf(err) != "AccessDenied" || !strings.Contains(err.Error(), "IamReady") {
		t.Fatalf("err = %v (code %q)", err, engine.CodeOf(err))
	}
	if len(r.cluster.Envs) != 1 {
		t.Error("the cluster was not applied after the IAM failure")
	}
	if c, ok := condition(out, domain.CondNamespaceReady); !ok || c.Status != domain.ConditionTrue {
		t.Errorf("NamespaceReady = %+v", c)
	}
	if _, ok := condition(out, domain.CondIamReady); ok {
		t.Error("IamReady reported although IAM failed")
	}
	if _, ok := condition(out, "Ready"); ok {
		t.Error("Ready reported although IAM failed")
	}
	// Even though IAM failed, the SecretStore can already name the team role.
	if r.cluster.Envs[0].TeamRoleARN == "" {
		t.Error("team role ARN missing")
	}
}

func TestClusterFailureReportsNamespaceReady(t *testing.T) {
	r := newRig(t, desired(idA, "payments", sonnet))
	r.cluster.Err = &engine.CodedError{Code: "Forbidden", Err: errors.New("forbidden")}
	r.cluster.Changed = []string{"Namespace/afs-t-payments"}
	out, err := r.c.Reconcile(t.Context(), ref(idA))
	if engine.CodeOf(err) != "Forbidden" || !strings.Contains(err.Error(), "NamespaceReady") {
		t.Fatalf("err = %v", err)
	}
	if c, ok := condition(out, domain.CondIamReady); !ok || c.Status != domain.ConditionTrue {
		t.Errorf("IamReady = %+v", c)
	}
	if len(out.Changed) != 1 {
		t.Errorf("what changed before the failure is still reported: %v", out.Changed)
	}
}

func TestAWSStepFailures(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*rig)
		want string
	}{
		{"profile", func(r *rig) { r.profiles.EnsureEr = teamtest.ErrBoom }, "inference profile for"},
		{"record arn", func(r *rig) { r.src.SetARNErr = teamtest.ErrBoom }, "record inference profile"},
		{"list profiles", func(r *rig) { r.profiles.ListErr = teamtest.ErrBoom }, "list inference profiles"},
		{"team role", func(r *rig) { r.iam.Err, r.iam.ErrRole = teamtest.ErrBoom, "afs-01JD0000000000000000000001" }, "team role"},
		{"bedrock role", func(r *rig) { r.iam.Err, r.iam.ErrRole = teamtest.ErrBoom, "afs-01JD0000000000000000000001-bedrock" }, "bedrock role"},
	}
	for _, tt := range tests {
		r := newRig(t, desired(idA, "payments", sonnet))
		tt.mut(r)
		if _, err := r.c.Reconcile(t.Context(), ref(idA)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v", tt.name, err)
		}
	}
	// a stale profile that cannot be deleted
	r := newRig(t, desired(idA, "payments", sonnet, "anthropic.claude-haiku-4-5-v1:0"))
	_, _ = r.c.Reconcile(t.Context(), ref(idA))
	r.src.Put(desired(idA, "payments", sonnet))
	r.profiles.DeleteEr = teamtest.ErrBoom
	if _, err := r.c.Reconcile(t.Context(), ref(idA)); err == nil || !strings.Contains(err.Error(), "delete inference profile") {
		t.Errorf("delete failure: %v", err)
	}
}

func TestMissingAndDeletedTeams(t *testing.T) {
	r := newRig(t)
	out, err := r.c.Reconcile(t.Context(), ref(idA))
	if err != nil || !out.NoStatus {
		t.Errorf("missing team: %+v, %v", out, err)
	}
	gone := desired(idA, "gone")
	gone.State = "deleted"
	r.src.Put(gone)
	out, err = r.c.Reconcile(t.Context(), ref(idA))
	if err != nil || !out.NoStatus || len(r.cluster.Envs) != 0 {
		t.Errorf("deleted team: %+v, %v", out, err)
	}
}

type failingSource struct{ *teamtest.Source }

func (failingSource) LoadTeam(context.Context, string) (team.Desired, error) {
	return team.Desired{}, teamtest.ErrBoom
}

func TestSourceErrorIsReturned(t *testing.T) {
	r := newRig(t)
	c, _ := team.NewController(team.Config{Source: failingSource{r.src}, Cluster: r.cluster, IAM: r.iam, Profiles: r.profiles,
		AWS: team.AWSConfig{Partition: "aws", Region: "r", AccountID: "1", GatewayRoleARN: gw, SecretsPrefix: "afs"}})
	if _, err := c.Reconcile(t.Context(), ref(idA)); !errors.Is(err, teamtest.ErrBoom) {
		t.Errorf("err = %v", err)
	}
}

func TestDeletingAndSuspendedHooks(t *testing.T) {
	d := desired(idA, "payments", sonnet)
	d.State = "deleting"
	r := newRig(t, d)
	out, err := r.c.Reconcile(t.Context(), ref(idA))
	if err != nil || out.Phase != domain.PhaseDeleting || len(r.cluster.Envs) != 0 {
		t.Errorf("deleting without a hook: %+v, %v", out, err)
	}

	var gotDeleting, gotSuspended team.Desired
	cfg := team.Config{Source: r.src, Cluster: r.cluster, IAM: r.iam, Profiles: r.profiles,
		AWS: team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: gw, SecretsPrefix: "afs"},
		Hooks: team.Hooks{
			Deleting: func(_ context.Context, t team.Desired) (engine.Outcome, error) {
				gotDeleting = t
				return engine.Outcome{Phase: domain.PhaseDeleting, Generation: t.Generation}, nil
			},
			Suspended: func(_ context.Context, t team.Desired) error { gotSuspended = t; return nil },
		}}
	c, err := team.NewController(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := c.Reconcile(t.Context(), ref(idA)); err != nil || out.Phase != domain.PhaseDeleting || gotDeleting.ID != idA || len(r.cluster.Envs) != 0 {
		t.Errorf("deleting hook: %+v, %v", out, err)
	}

	s := desired(idA, "payments", sonnet)
	s.State = "suspended"
	r.src.Put(s)
	out, err = c.Reconcile(t.Context(), ref(idA))
	if err != nil || gotSuspended.ID != idA || len(r.cluster.Envs) != 1 {
		t.Errorf("suspended hook: %+v, %v", out, err)
	}
	if _, ok := condition(out, "Ready"); !ok {
		t.Error("a suspended team that reconciled is still Ready")
	}
	cfg.Hooks.Suspended = func(context.Context, team.Desired) error { return teamtest.ErrBoom }
	c2, _ := team.NewController(cfg)
	if _, err := c2.Reconcile(t.Context(), ref(idA)); !errors.Is(err, teamtest.ErrBoom) {
		t.Errorf("suspend hook error: %v", err)
	}
}

func TestPlatformController(t *testing.T) {
	if _, err := team.NewPlatformController(nil, team.PlatformSpec{}); err == nil {
		t.Error("nil cluster accepted")
	}
	cl := &teamtest.Cluster{Changed: []string{"CiliumNetworkPolicy/afs-system/p"}}
	pc, err := team.NewPlatformController(cl, team.PlatformSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if pc.Kind() != engine.KindPlatform || team.PlatformRef.Kind != engine.KindPlatform {
		t.Error("kind")
	}
	out, err := pc.Reconcile(t.Context(), team.PlatformRef)
	if err != nil || !out.NoStatus || len(out.Changed) != 1 {
		t.Errorf("outcome = %+v, %v", out, err)
	}
	if got := cl.Platforms[0]; got.GatewayNamespace != "afs-system" || got.GatewayName != "afs-gateway" || got.GatewayPort != 8443 {
		t.Errorf("defaults not applied: %+v", got)
	}
	cl.Err = errors.New("apply failed")
	if _, err := pc.Reconcile(t.Context(), team.PlatformRef); err == nil || !strings.Contains(err.Error(), "PlatformReady") {
		t.Errorf("err = %v", err)
	}
	// explicit spec kept
	pc2, _ := team.NewPlatformController(cl, team.PlatformSpec{GatewayNamespace: "gw", GatewayName: "g", GatewayPort: 9})
	cl.Err = nil
	_, _ = pc2.Reconcile(t.Context(), team.PlatformRef)
	if got := cl.Platforms[len(cl.Platforms)-1]; got.GatewayNamespace != "gw" || got.GatewayPort != 9 {
		t.Errorf("spec = %+v", got)
	}
}

// MOD-7: a model that stopped being callable keeps its profile for the grace; the row turns retiring, a retiring one
// is kept (and requeued) until its time, and one past its time is deleted with its row.
func TestRetiringProfilesFollowTheGrace(t *testing.T) {
	r := newRig(t, desired(idA, "payments", sonnet))
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	arnFor := func(m string) string {
		return "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/" + m
	}
	d := desired(idA, "payments", sonnet)
	d.Retiring = []team.RetiringModel{
		{ID: "m.fresh", ProfileARN: arnFor("fresh")},
		{ID: "m.waiting", ProfileARN: arnFor("waiting"), RetireAfter: now.Add(48 * time.Hour)},
		{ID: "m.expired", ProfileARN: arnFor("expired"), RetireAfter: now.Add(-time.Hour)},
		{ID: "m.expired-noarn", RetireAfter: now.Add(-time.Minute)},
	}
	r.src.Put(d)
	cfg := team.Config{Source: r.src, Cluster: r.cluster, IAM: r.iam, Profiles: r.profiles, Now: func() time.Time { return now },
		AWS: team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: gw,
			PermissionsBoundaryARN: "arn:aws:iam::111122223333:policy/afs-boundary", ClusterARN: "arn:aws:eks:us-east-1:111122223333:cluster/afs", SecretsPrefix: "afs"}}
	c, err := team.NewController(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Reconcile(t.Context(), ref(idA))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.src.Retired[idA+"/m.fresh"]; !ok || len(r.src.Retired) != 1 {
		t.Errorf("retired: %v", r.src.Retired)
	}
	if len(r.src.Dropped) != 2 {
		t.Errorf("dropped: %v", r.src.Dropped)
	}
	if out.Requeue <= 0 || out.Requeue > team.ProfileGrace {
		t.Errorf("requeue = %v", out.Requeue)
	}
	b, _ := r.iam.Role("afs-01JD0000000000000000000001-bedrock")
	if !strings.Contains(b.Policies["bedrock"], "waiting") || strings.Contains(b.Policies["bedrock"], "expired") {
		t.Errorf("grants: %s", b.Policies["bedrock"])
	}
}
