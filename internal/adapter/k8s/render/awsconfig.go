package render

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Files of the AWS and tag delivery (RC-US-2.1).
const (
	AWSConfigPath = "/etc/afs/aws/config"
	TagsPath      = "/etc/afs/tags.json"
	// ModelsPath is the list of models the agent or pool may call and the default (MOD-6).
	ModelsPath = "/etc/afs/models.json"
	modelsKey  = "models.json"
	// RoutesPath is /etc/afs/llm-routes.json (gateway contract §12.4), present on team gateways only.
	RoutesPath   = "/etc/afs/llm-routes.json"
	routesKey    = "llm-routes.json"
	awsConfigKey = "config"
	tagsKey      = "tags.json"
)

// AWSConfig renders /etc/afs/aws/config: one profile per link that is not disabled. Every profile sets
// role_session_name to the agent's id, so a CloudTrail event in the linked account names the agent (D51);
// the profile's source credentials are the pod's own (Pod Identity, through the container credential
// provider), and the link's external id is sent on AssumeRole. It returns "" when there is no link.
func AWSConfig(d agent.Desired) string {
	var links []agent.RoleLink
	for _, l := range d.RoleLinks {
		if l.Status != "disabled" {
			links = append(links, l)
		}
	}
	if len(links) == 0 {
		return ""
	}
	sort.Slice(links, func(i, j int) bool { return links[i].ProfileName < links[j].ProfileName })
	var b strings.Builder
	for i, l := range links {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[profile %s]\nrole_arn = %s\nexternal_id = %s\nrole_session_name = %s\ncredential_source = EcsContainer\n", l.ProfileName, l.RoleARN, l.ExternalID, d.ID)
		if l.MaxSessionSeconds > 0 {
			fmt.Fprintf(&b, "duration_seconds = %d\n", l.MaxSessionSeconds)
		}
	}
	return b.String()
}

// TagsJSON is the agent's effective tags as one JSON object with sorted keys: the content of
// /etc/afs/tags.json and of AFS_TAGS (TAG-4).
func TagsJSON(d agent.Desired) string {
	tags := d.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	b, _ := json.Marshal(tags) // a map of strings always marshals; keys come out sorted
	return string(b)
}

// ModelsJSON is the content of /etc/afs/models.json: {"default": "<id>", "models": ["<id>", ...]}, the models
// sorted. Ids only: no price, profile or account detail reaches the pod.
func ModelsJSON(def string, ids []string) string {
	ms := append([]string{}, ids...)
	sort.Strings(ms)
	b, _ := json.Marshal(struct {
		Default string   `json:"default"`
		Models  []string `json:"models"`
	}{def, ms})
	return string(b)
}

// addRoutes puts llm-routes.json in a ConfigMap's data while the team is on its own gateway.
func addRoutes(data map[string]any, t agent.Team, r agent.LLMRoutes) {
	if agent.Delivered(t) {
		data[routesKey] = agent.RoutesJSON(agent.Gateway{}.For(t).URL(), r)
	}
}

// routeDelivery appends the route variables after the gateway variables (they reference $(AFS_GATEWAY_URL)) and
// mounts /etc/afs/llm-routes.json while the team is on its own gateway.
func routeDelivery(t agent.Team, r agent.LLMRoutes, envVars []corev1.EnvVar, items []corev1.KeyToPath, mounts []corev1.VolumeMount) (
	[]corev1.EnvVar, []corev1.KeyToPath, []corev1.VolumeMount) {
	if !agent.Delivered(t) {
		return envVars, items, mounts
	}
	for _, e := range agent.RouteEnv(t, r) {
		envVars = append(envVars, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	return envVars, append(items, corev1.KeyToPath{Key: routesKey, Path: routesKey}),
		append(mounts, corev1.VolumeMount{Name: volumeAWS, MountPath: RoutesPath, SubPath: routesKey, ReadOnly: true})
}

// ModelsEnv is AFS_MODELS: the ids, sorted, comma separated.
func ModelsEnv(ids []string) string {
	ms := append([]string{}, ids...)
	sort.Strings(ms)
	return strings.Join(ms, ",")
}

// AgentAWSConfigMap renders the ConfigMap that holds /etc/afs/tags.json, /etc/afs/models.json and, when the agent has links,
// /etc/afs/aws/config.
func AgentAWSConfigMap(d agent.Desired) *unstructured.Unstructured {
	u := New("v1", "ConfigMap", d.Team.Namespace, agent.AWSConfigMapName(d.Name))
	data := map[string]any{tagsKey: TagsJSON(d), modelsKey: ModelsJSON(d.ModelID, d.Models)}
	addRoutes(data, d.Team, d.Routes)
	if cfg := AWSConfig(d); cfg != "" {
		data[awsConfigKey] = cfg
	}
	u.Object["data"] = data
	return Managed(u, OwnerOfAgent(d))
}
