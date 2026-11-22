//go:build envtest

package k8s_test

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

const (
	egTeam = "team_01JD0000000000000000000071"
	egAgtA = "agt_01JD0000000000000000000081"
	egAgtB = "agt_01JD0000000000000000000082"
)

type liveEgress struct {
	mu sync.Mutex
	d  egress.Desired
}

func (s *liveEgress) set(agents ...egress.Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Agents = agents
}

func (s *liveEgress) LoadEgress(context.Context, string) (egress.Desired, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d, nil
}

type recordedConds struct {
	mu  sync.Mutex
	got map[string]domain.Condition
}

func (r *recordedConds) SetCondition(_ context.Context, id string, c domain.Condition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[string]domain.Condition{}
	}
	r.got[id] = c
	return nil
}

// M2-H1: the egress controller with the real Kubernetes adapter renders one Cilium policy per agent,
// takes a system's FQDNs out of it when the system is switched off at any level (well inside 60 s and
// without touching a pod), removes the policy of a deleted agent, never deletes one it does not own,
// and reports EgressPolicyReady (RC-US-5.1, EXT-5).
func TestEgressPoliciesFollowCatalog(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	envtesting.EnsureNamespace(t, env.Config, "afs-t-eg", nil)

	src := &liveEgress{d: egress.Desired{TeamID: egTeam, Namespace: "afs-t-eg", State: "active"}}
	conds := &recordedConds{}
	ctl, err := egress.NewController(egress.Config{Source: src, Cluster: k8s.NewEgressCluster(c), Agents: conds})
	if err != nil {
		t.Fatal(err)
	}
	ref := engine.NewRef(egress.Kind, egTeam, egTeam)
	reconcile := func() engine.Outcome {
		t.Helper()
		out, err := ctl.Reconcile(ctx, ref)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return out
	}
	gh := func(off string) egress.System {
		return egress.System{ID: "ext_gh", Slug: "github", Category: "dev_tools", Endpoints: []string{"api.github.com", "*.githubusercontent.com"}, DisabledAt: off}
	}
	slack := egress.System{ID: "ext_slack", Slug: "slack", Category: "collaboration", Endpoints: []string{"slack.com:443"}}
	agent := func(id string, systems ...egress.System) egress.Agent {
		return egress.Agent{ID: id, State: "enabled", Systems: systems}
	}
	policy := func(id string) *unstructured.Unstructured {
		o, err := c.Get(ctx, gvkCNP, "afs-t-eg", render.EgressPolicyName(id))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	matchers := func(id string) []string {
		o := policy(id)
		if o == nil {
			return nil
		}
		rules, _, _ := unstructured.NestedSlice(o.Object, "spec", "egress")
		var out []string
		for _, r := range rules {
			fq, _ := r.(map[string]any)["toFQDNs"].([]any)
			for _, f := range fq {
				for _, v := range f.(map[string]any) {
					out = append(out, v.(string))
				}
			}
		}
		return out
	}
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}

	src.set(agent(egAgtA, gh(""), slack), agent(egAgtB, gh(""), slack))
	if out := reconcile(); len(out.Changed) != 2 {
		t.Fatalf("first pass changed %v", out.Changed)
	}
	for _, id := range []string{egAgtA, egAgtB} {
		m := matchers(id)
		if !has(m, "api.github.com") || !has(m, "*.githubusercontent.com") || !has(m, "slack.com") {
			t.Fatalf("%s matchers = %v", id, m)
		}
		sel, _, _ := unstructured.NestedString(policy(id).Object, "spec", "endpointSelector", "matchLabels", render.LabelAgentID)
		if sel != id {
			t.Errorf("selector of %s = %q", id, sel)
		}
	}
	if c := conds.got[egAgtA]; c.Type != domain.CondEgressPolicyReady || c.Status != domain.ConditionTrue {
		t.Errorf("condition = %+v", c)
	}
	if out := reconcile(); len(out.Changed) != 0 {
		t.Fatalf("idempotent pass changed %v", out.Changed)
	}

	// Switching GitHub off for agent B (an agent-level opt-out) changes only B's policy.
	start := time.Now()
	src.set(agent(egAgtA, gh(""), slack), agent(egAgtB, gh(egress.OffAgent), slack))
	if out := reconcile(); len(out.Changed) != 1 {
		t.Fatalf("opt-out pass changed %v", out.Changed)
	}
	if time.Since(start) > 60*time.Second {
		t.Fatal("took more than 60 s")
	}
	if has(matchers(egAgtB), "api.github.com") || !has(matchers(egAgtB), "slack.com") || !has(matchers(egAgtA), "api.github.com") {
		t.Fatalf("A=%v B=%v", matchers(egAgtA), matchers(egAgtB))
	}
	// Off at the service level takes it from both; with every system off the policy stays and allows nothing.
	src.set(agent(egAgtA, gh(egress.OffService)), agent(egAgtB, gh(egress.OffService)))
	reconcile()
	for _, id := range []string{egAgtA, egAgtB} {
		if m := matchers(id); len(m) != 0 {
			t.Errorf("%s still allows %v with every system off", id, m)
		}
		if policy(id) == nil {
			t.Errorf("the policy of %s must stay (it keeps the agent selected)", id)
		}
	}

	// A policy of the reconciler for an agent that is gone is pruned; an unmanaged one is not.
	foreign := render.New("cilium.io/v2", "CiliumNetworkPolicy", "afs-t-eg", "someone-elses")
	foreign.Object["spec"] = map[string]any{"endpointSelector": map[string]any{}, "egress": []any{map[string]any{}}}
	if _, err := c.Dynamic().Resource(schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}).Namespace("afs-t-eg").Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	src.set(agent(egAgtA, gh(""), slack))
	reconcile()
	if policy(egAgtB) != nil {
		t.Error("the policy of the removed agent survived")
	}
	if policy(egAgtA) == nil {
		t.Error("the policy of a live agent was pruned")
	}
	if o, _ := c.Get(ctx, gvkCNP, "afs-t-eg", "someone-elses"); o == nil {
		t.Error("an unmanaged policy was deleted")
	}
}
