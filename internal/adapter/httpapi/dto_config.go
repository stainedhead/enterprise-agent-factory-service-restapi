package httpapi

import "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"

// namespaceConfigBody is a team namespace configuration value (API contract §5 ConfigValue).
type namespaceConfigBody struct {
	Name      string   `json:"name"`
	Value     string   `json:"value"`
	Source    string   `json:"source"`
	UsedBy    []string `json:"usedBy"`
	Version   int64    `json:"version"`
	UpdatedAt string   `json:"updatedAt"`
}

func toNamespaceConfigBody(v domain.NamespaceConfigValue) namespaceConfigBody {
	used := v.UsedBy
	if used == nil {
		used = []string{}
	}
	return namespaceConfigBody{Name: string(v.Name), Value: v.Value, Source: string(domain.ConfigSourceLiteral), UsedBy: used,
		Version: int64(v.Version), UpdatedAt: w2Time(v.UpdatedAt)}
}

// agentConfigBody is one entry of an agent's configuration: its own value or a link to the
// namespace value, with the effective value resolved.
type agentConfigBody struct {
	Name          string  `json:"name"`
	Source        string  `json:"source"`
	Value         *string `json:"value"`
	ResolvedValue *string `json:"resolvedValue"`
	Version       int64   `json:"version"`
	UpdatedAt     string  `json:"updatedAt"`
}

func toAgentConfigBody(v domain.AgentConfigValue) agentConfigBody {
	return agentConfigBody{Name: string(v.Name), Source: string(v.Source), Value: v.Value, ResolvedValue: v.ResolvedValue,
		Version: int64(v.Version), UpdatedAt: w2Time(v.UpdatedAt)}
}
