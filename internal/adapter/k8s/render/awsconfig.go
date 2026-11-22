package render

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Files of the AWS and tag delivery (RC-US-2.1).
const (
	AWSConfigPath = "/etc/afs/aws/config"
	TagsPath      = "/etc/afs/tags.json"
	awsConfigKey  = "config"
	tagsKey       = "tags.json"
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

// AgentAWSConfigMap renders the ConfigMap that holds /etc/afs/tags.json and, when the agent has links,
// /etc/afs/aws/config.
func AgentAWSConfigMap(d agent.Desired) *unstructured.Unstructured {
	u := New("v1", "ConfigMap", d.Team.Namespace, agent.AWSConfigMapName(d.Name))
	data := map[string]any{tagsKey: TagsJSON(d)}
	if cfg := AWSConfig(d); cfg != "" {
		data[awsConfigKey] = cfg
	}
	u.Object["data"] = data
	return Managed(u, OwnerOfAgent(d))
}
