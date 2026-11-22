package render_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
)

func TestEgressPolicyName(t *testing.T) {
	got := render.EgressPolicyName("agt_01JD0000000000000000000061")
	if got != "afs-egress-agt-01jd0000000000000000000061" {
		t.Errorf("name = %q", got)
	}
}

func fqdns(t *testing.T, u *unstructured.Unstructured) map[string][]any {
	t.Helper()
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "egress")
	out := map[string][]any{}
	for _, r := range rules {
		m := r.(map[string]any)
		ports, _ := m["toPorts"].([]any)
		if len(ports) == 0 {
			out["none"] = nil
			continue
		}
		port := ports[0].(map[string]any)["ports"].([]any)[0].(map[string]any)["port"].(string)
		out[port] = m["toFQDNs"].([]any)
	}
	return out
}

func TestCiliumAgentEgress(t *testing.T) {
	const team, agent = "team_01JD0000000000000000000051", "agt_01JD0000000000000000000061"
	ep := func(h string, p int) egress.Endpoint { return egress.Endpoint{Host: h, Port: p} }
	for _, tc := range []struct {
		name  string
		rules []egress.Rule
		want  map[string][]any
	}{
		{
			name: "names, patterns and ports",
			rules: []egress.Rule{
				{Slug: "github", Endpoints: []egress.Endpoint{ep("api.github.com", 443), ep("git.example.com", 8443)}},
				{Slug: "slack", Endpoints: []egress.Endpoint{ep("*.slack.com", 443), ep("api.github.com", 443)}},
			},
			want: map[string][]any{
				"443":  {map[string]any{"matchName": "api.github.com"}, map[string]any{"matchPattern": "*.slack.com"}},
				"8443": {map[string]any{"matchName": "git.example.com"}},
			},
		},
		{name: "no system on allows nothing more", rules: nil, want: map[string][]any{"none": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := render.CiliumAgentEgress("afs-t-eg", egress.AgentPolicy{TeamID: team, AgentID: agent, Rules: tc.rules})
			if got := fqdns(t, u); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rules = %v, want %v", got, tc.want)
			}
			sel, _, _ := unstructured.NestedString(u.Object, "spec", "endpointSelector", "matchLabels", render.LabelAgentID)
			if sel != agent {
				t.Errorf("selector = %q", sel)
			}
			if !render.IsManaged(u) || render.OwnerOf(u) != render.OwnerOfEgress(team) || u.GetLabels()[render.LabelAgentID] != agent {
				t.Errorf("labels = %v", u.GetLabels())
			}
			if u.GetNamespace() != "afs-t-eg" || u.GetKind() != "CiliumNetworkPolicy" {
				t.Errorf("object = %s/%s", u.GetNamespace(), u.GetKind())
			}
		})
	}
}
