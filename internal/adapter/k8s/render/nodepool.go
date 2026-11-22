package render

import (
	"maps"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// The dedicated tier's node objects (TEAM-6, RC-US-2.6).
const (
	karpenterAPI = "karpenter.sh/v1"
	nodeClassAPI = "karpenter.k8s.aws/v1"
	// NodePoolNamePrefix prefixes the NodePool and EC2NodeClass of a team: "afs-t-<slug>".
	NodePoolNamePrefix = "afs-t-"
)

// NodePoolConfig is what the platform supplies for every dedicated pool (Helm values).
type NodePoolConfig struct {
	// NodeClass is the base EC2NodeClass spec (role, subnet and security group selectors, AMI family)
	// every team's class copies; the team's tags are merged into it.
	NodeClass map[string]any
}

// NodePoolOwner owns the node objects of a team.
func NodePoolOwner(d agent.Desired) Owner {
	return Owner{Kind: string(engine.KindTeam), ID: d.Team.ID, TeamID: d.Team.ID, TeamSlug: d.Team.Slug}
}

// NodePoolName is the NodePool and EC2NodeClass name of a team.
func NodePoolName(slug string) string { return NodePoolNamePrefix + slug }

// NodeClass renders the team's EC2NodeClass: the platform's base spec with the team's tags, so every node
// and volume of the team's dedicated pool carries them (TEAM-6, TAG-4).
func NodeClass(d agent.Desired, tags map[string]string, cfg NodePoolConfig) *unstructured.Unstructured {
	u := New(nodeClassAPI, "EC2NodeClass", "", NodePoolName(d.Team.Slug))
	spec := map[string]any{}
	for k, v := range cfg.NodeClass {
		spec[k] = v
	}
	t := map[string]any{}
	for k, v := range maps.All(tags) {
		t[k] = v
	}
	spec["tags"] = t
	u.Object["spec"] = spec
	return Managed(u, NodePoolOwner(d))
}

// NodePool renders the team's Karpenter NodePool: nodes labelled and tainted with the team's id, so only the
// team's pods (which tolerate and select it, and which admission lets only this team do) land there and no
// other team's pod does.
func NodePool(d agent.Desired) *unstructured.Unstructured {
	u := New(karpenterAPI, "NodePool", "", NodePoolName(d.Team.Slug))
	u.Object["spec"] = map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{DedicatedNodeKey: d.Team.ID}},
			"spec": map[string]any{
				"nodeClassRef": map[string]any{"group": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": NodePoolName(d.Team.Slug)},
				"taints":       []any{map[string]any{"key": DedicatedNodeKey, "value": d.Team.ID, "effect": "NoSchedule"}},
				"requirements": []any{
					map[string]any{"key": "kubernetes.io/arch", "operator": "In", "values": []any{"amd64", "arm64"}},
					map[string]any{"key": "karpenter.sh/capacity-type", "operator": "In", "values": []any{"on-demand"}},
				},
			},
		},
		"disruption": map[string]any{"consolidationPolicy": "WhenEmptyOrUnderutilized", "consolidateAfter": "5m"},
	}
	return Managed(u, NodePoolOwner(d))
}
