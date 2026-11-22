package egress_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

const (
	teamID = "team_01JD0000000000000000000051"
	agtA   = "agt_01JD0000000000000000000061"
	agtB   = "agt_01JD0000000000000000000062"
)

func TestParseEndpoint(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    egress.Endpoint
		wantErr bool
	}{
		{in: "api.github.com", want: egress.Endpoint{Host: "api.github.com", Port: 443}},
		{in: "git.example.com:8443", want: egress.Endpoint{Host: "git.example.com", Port: 8443}},
		{in: "*.slack.com", want: egress.Endpoint{Host: "*.slack.com", Port: 443}},
		{in: "*.slack.com:80", want: egress.Endpoint{Host: "*.slack.com", Port: 80}},
		{in: "localhost", wantErr: true},
		{in: "API.example.com", wantErr: true},
		{in: "https://example.com", wantErr: true},
		{in: "example.com/path", wantErr: true},
		{in: "a.*.example.com", wantErr: true},
		{in: "*.", wantErr: true},
		{in: "-a.example.com", wantErr: true},
		{in: "a..example.com", wantErr: true},
		{in: "example.com:0", wantErr: true},
		{in: "example.com:70000", wantErr: true},
		{in: "example.com:http", wantErr: true},
		{in: "user@example.com", wantErr: true},
		{in: "", wantErr: true},
	} {
		got, err := egress.ParseEndpoint(tc.in)
		if (err != nil) != tc.wantErr || (err == nil && got != tc.want) {
			t.Errorf("ParseEndpoint(%q) = %+v, %v; want %+v, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
	if !(egress.Endpoint{Host: "*.a.com"}).Wildcard() || (egress.Endpoint{Host: "a.com"}).Wildcard() {
		t.Error("Wildcard")
	}
}

func sys(slug, off string, eps ...string) egress.System {
	return egress.System{ID: "ext_" + slug, Slug: slug, Category: "dev_tools", Endpoints: eps, DisabledAt: off}
}

func TestPolicyFor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		systems []egress.System
		slugs   []string
		wantErr bool
	}{
		{name: "all on, sorted by slug", systems: []egress.System{sys("slack", "", "*.slack.com"), sys("github", "", "api.github.com")}, slugs: []string{"github", "slack"}},
		{name: "off at service level", systems: []egress.System{sys("github", egress.OffService, "api.github.com"), sys("slack", "", "*.slack.com")}, slugs: []string{"slack"}},
		{name: "off at team level", systems: []egress.System{sys("github", egress.OffTeam, "api.github.com")}},
		{name: "off at agent level", systems: []egress.System{sys("github", egress.OffAgent, "api.github.com")}},
		{name: "none", systems: nil},
		{name: "bad endpoint is reported, good ones kept", systems: []egress.System{sys("github", "", "api.github.com", "Bad Host")}, slugs: []string{"github"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := egress.PolicyFor(teamID, egress.Agent{ID: agtA, State: "enabled", Systems: tc.systems})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			var got []string
			for _, r := range p.Rules {
				got = append(got, r.Slug)
			}
			if !reflect.DeepEqual(got, tc.slugs) {
				t.Errorf("slugs = %v, want %v", got, tc.slugs)
			}
			if p.AgentID != agtA || p.TeamID != teamID {
				t.Errorf("policy identity = %+v", p)
			}
		})
	}
}

type fakeSource struct {
	d   egress.Desired
	err error
}

func (s *fakeSource) LoadEgress(context.Context, string) (egress.Desired, error) { return s.d, s.err }

type fakeCluster struct {
	applied   map[string]egress.AgentPolicy
	pruned    []string
	applyErr  map[string]error
	pruneErr  error
	namespace string
}

func (c *fakeCluster) ApplyAgentPolicy(_ context.Context, ns string, p egress.AgentPolicy) (egress.ApplyResult, error) {
	c.namespace = ns
	if err := c.applyErr[p.AgentID]; err != nil {
		return egress.ApplyResult{}, err
	}
	if c.applied == nil {
		c.applied = map[string]egress.AgentPolicy{}
	}
	c.applied[p.AgentID] = p
	return egress.ApplyResult{Changed: []string{"CiliumNetworkPolicy/" + ns + "/" + p.AgentID}}, nil
}

func (c *fakeCluster) PruneAgentPolicies(_ context.Context, _, _ string, keep []string) (egress.ApplyResult, error) {
	c.pruned = keep
	return egress.ApplyResult{}, c.pruneErr
}

type fakeAgents struct{ got map[string]domain.Condition }

func (f *fakeAgents) SetCondition(_ context.Context, id string, c domain.Condition) error {
	if f.got == nil {
		f.got = map[string]domain.Condition{}
	}
	f.got[id] = c
	return nil
}

func desired(state string, agents ...egress.Agent) egress.Desired {
	return egress.Desired{TeamID: teamID, Namespace: "afs-t-eg", State: state, Agents: agents}
}

func TestNewControllerRequiresPorts(t *testing.T) {
	if _, err := egress.NewController(egress.Config{}); err == nil {
		t.Fatal("want error without Source and Cluster")
	}
	c, err := egress.NewController(egress.Config{Source: &fakeSource{}, Cluster: &fakeCluster{}})
	if err != nil || c.Kind() != egress.Kind {
		t.Fatalf("NewController = %v, %v", c, err)
	}
}

func TestReconcile(t *testing.T) {
	ref := engine.NewRef(egress.Kind, teamID, teamID)
	gh, slack := sys("github", "", "api.github.com"), sys("slack", "", "*.slack.com")
	for _, tc := range []struct {
		name       string
		src        *fakeSource
		cluster    *fakeCluster
		wantErr    bool
		terminal   bool
		wantStatus bool // a status is written
		applied    []string
		pruneKeep  []string
		condA      string // agtA's EgressPolicyReady status
	}{
		{
			name: "applies a policy per live agent and prunes the rest",
			src: &fakeSource{d: desired("active",
				egress.Agent{ID: agtA, State: "enabled", Systems: []egress.System{gh, slack}},
				egress.Agent{ID: agtB, State: "deleted", Systems: []egress.System{gh}})},
			cluster: &fakeCluster{}, wantStatus: true, applied: []string{agtA}, pruneKeep: []string{agtA}, condA: "True",
		},
		{
			name:    "a paused agent keeps its policy",
			src:     &fakeSource{d: desired("active", egress.Agent{ID: agtA, State: "paused", Systems: []egress.System{gh}})},
			cluster: &fakeCluster{}, wantStatus: true, applied: []string{agtA}, pruneKeep: []string{agtA}, condA: "True",
		},
		{
			name:    "a suspended team keeps its policies",
			src:     &fakeSource{d: desired("suspended", egress.Agent{ID: agtA, State: "enabled", Systems: []egress.System{gh}})},
			cluster: &fakeCluster{}, wantStatus: true, applied: []string{agtA}, pruneKeep: []string{agtA}, condA: "True",
		},
		{
			name:    "a deleting team writes nothing",
			src:     &fakeSource{d: desired("deleting", egress.Agent{ID: agtA, State: "enabled", Systems: []egress.System{gh}})},
			cluster: &fakeCluster{},
		},
		{name: "a missing team writes nothing", src: &fakeSource{err: domain.NotFound("team")}, cluster: &fakeCluster{}},
		{name: "a source failure is retried", src: &fakeSource{err: errors.New("db down")}, cluster: &fakeCluster{}, wantErr: true, wantStatus: true},
		{
			name:    "an apply failure is a condition error and does not stop other agents",
			src:     &fakeSource{d: desired("active", egress.Agent{ID: agtA, State: "enabled", Systems: []egress.System{gh}}, egress.Agent{ID: agtB, State: "enabled", Systems: []egress.System{slack}})},
			cluster: &fakeCluster{applyErr: map[string]error{agtA: errors.New("apiserver down")}},
			wantErr: true, wantStatus: true, applied: []string{agtB}, pruneKeep: []string{agtA, agtB}, condA: "False",
		},
		{
			name:    "a prune failure is retried",
			src:     &fakeSource{d: desired("active", egress.Agent{ID: agtA, State: "enabled", Systems: []egress.System{gh}})},
			cluster: &fakeCluster{pruneErr: errors.New("list failed")}, wantErr: true, wantStatus: true, applied: []string{agtA}, pruneKeep: []string{agtA}, condA: "True",
		},
		{
			name:    "an unrenderable endpoint is terminal and still applies the rest",
			src:     &fakeSource{d: desired("active", egress.Agent{ID: agtA, State: "enabled", Systems: []egress.System{sys("bad", "", "NOT A HOST")}}, egress.Agent{ID: agtB, State: "enabled", Systems: []egress.System{slack}})},
			cluster: &fakeCluster{}, wantErr: true, terminal: true, wantStatus: true, applied: []string{agtB}, pruneKeep: []string{agtA, agtB}, condA: "False",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := &fakeAgents{}
			c, err := egress.NewController(egress.Config{Source: tc.src, Cluster: tc.cluster, Agents: agents})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Reconcile(t.Context(), ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.terminal && !errors.Is(err, engine.ErrTerminal) {
				t.Errorf("err = %v, want terminal", err)
			}
			if !tc.wantStatus && !out.NoStatus {
				t.Errorf("outcome %+v, want NoStatus", out)
			}
			var applied []string
			for id := range tc.cluster.applied {
				applied = append(applied, id)
			}
			sort.Strings(applied)
			if !reflect.DeepEqual(applied, tc.applied) {
				t.Errorf("applied = %v, want %v", applied, tc.applied)
			}
			if !reflect.DeepEqual(tc.cluster.pruned, tc.pruneKeep) {
				t.Errorf("prune keep = %v, want %v", tc.cluster.pruned, tc.pruneKeep)
			}
			if tc.condA != "" {
				if got := string(agents.got[agtA].Status); got != tc.condA || agents.got[agtA].Type != domain.CondEgressPolicyReady {
					t.Errorf("agent condition = %+v, want %s", agents.got[agtA], tc.condA)
				}
			}
			if !tc.wantErr && tc.wantStatus {
				if len(out.Conditions) != 1 || out.Conditions[0].Type != domain.CondEgressPolicyReady || out.Conditions[0].Status != domain.ConditionTrue {
					t.Errorf("conditions = %+v", out.Conditions)
				}
			}
		})
	}
}

// RC-US-5.1: switching a system off at each level removes its endpoint from the next applied policy.
func TestSwitchingOffRemovesEndpoints(t *testing.T) {
	ref := engine.NewRef(egress.Kind, teamID, teamID)
	src := &fakeSource{}
	cl := &fakeCluster{}
	c, _ := egress.NewController(egress.Config{Source: src, Cluster: cl})
	for _, off := range []string{"", egress.OffService, egress.OffTeam, egress.OffAgent} {
		src.d = desired("active", egress.Agent{ID: agtA, State: "enabled", Systems: []egress.System{sys("github", off, "api.github.com"), sys("slack", "", "*.slack.com")}})
		if _, err := c.Reconcile(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		p := cl.applied[agtA]
		hasGH := false
		for _, r := range p.Rules {
			hasGH = hasGH || r.Slug == "github"
		}
		if hasGH != (off == "") {
			t.Errorf("off=%q: github allowed = %v", off, hasGH)
		}
		if len(p.Rules) == 0 || p.Rules[len(p.Rules)-1].Slug != "slack" {
			t.Errorf("off=%q: slack must stay allowed: %+v", off, p.Rules)
		}
	}
}

func TestRoutes(t *testing.T) {
	reg, err := engine.NewRegistry(append(egress.Routes(), engine.Route{Table: "agents", Targets: []engine.Target{egress.AgentsTarget()}})...)
	if err != nil {
		t.Fatal(err)
	}
	refs, _ := reg.Resolve(t.Context(), engine.Change{Table: "external_system_optouts", TeamID: teamID, ID: "xso_1"}, nil)
	if len(refs) != 1 || refs[0] != engine.NewRef(egress.Kind, teamID, teamID) {
		t.Errorf("optout refs = %v", refs)
	}
	fan := func(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
		if len(kinds) != 1 || kinds[0] != egress.Kind {
			t.Errorf("fan kinds = %v", kinds)
		}
		return []engine.Ref{engine.NewRef(egress.Kind, teamID, teamID)}, nil
	}
	refs, _ = reg.Resolve(t.Context(), engine.Change{Table: "external_systems", ID: "ext_1"}, fan)
	if len(refs) != 1 {
		t.Errorf("service-level system change must fan out: %v", refs)
	}
	refs, _ = reg.Resolve(t.Context(), engine.Change{Table: "agents", TeamID: teamID, ID: agtA}, nil)
	if len(refs) != 1 || refs[0].ID != teamID {
		t.Errorf("agents change must reconcile the team's egress, got %v", refs)
	}
}
