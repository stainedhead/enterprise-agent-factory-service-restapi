package agent

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Model routes of an agent or pool (gateway contract §12.4, MEG-RC-3): the AFS_LLM_* and OPENAI_* variables and
// /etc/afs/llm-routes.json. Routes are served only by team gateways, so a team on the shared gateway gets none.

// GatewayPerNamespace is teams.gateway_deployment of a team that runs its own gateway.
const GatewayPerNamespace = "per_namespace"

// Team gateway location (gateway contract §12.1).
const (
	teamGatewayName = "afs-gateway"
	teamGatewayPort = 8443
)

// For returns the gateway the team's pods call: the team's own while it is per_namespace, g otherwise.
func (g Gateway) For(t Team) Gateway {
	if t.GatewayDeployment == GatewayPerNamespace {
		return Gateway{Namespace: t.Namespace, Name: teamGatewayName, Port: teamGatewayPort}
	}
	return g
}

// RouteModel is one model of a route.
type RouteModel struct {
	Name            string `json:"name"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
}

// RouteSpec is one effective route of the subject.
type RouteSpec struct {
	Slug      string
	Protocols []string
	Models    []RouteModel
	// Pricing is per_token, capacity or free.
	Pricing string
}

// LLMRoutes are the subject's effective routes and default route model.
type LLMRoutes struct {
	Routes []RouteSpec
	// DefaultRoute and DefaultModel are set when the default model is a route model.
	DefaultRoute, DefaultModel string
	// BedrockOn is the Bedrock provider switch (AFS_LLM_BEDROCK_BASE_URL).
	BedrockOn bool
}

// Slugs lists the effective route slugs, the default first.
func (r LLMRoutes) Slugs() []string {
	s := make([]string, 0, len(r.Routes))
	for _, rt := range r.Routes {
		s = append(s, rt.Slug)
	}
	return domain.OrderRoutes(s, r.DefaultRoute)
}

// Effective is the Bedrock model ids followed by each route model as <route>/<model>: the set the default
// model is checked against (ModelUnavailable).
func (r LLMRoutes) Effective(models []string) []string {
	out := slices.Clone(models)
	for _, rt := range r.Routes {
		for _, m := range rt.Models {
			out = append(out, rt.Slug+"/"+m.Name)
		}
	}
	return out
}

// Delivered reports whether the team's pods receive route variables: only on a team gateway.
func Delivered(t Team) bool { return t.GatewayDeployment == GatewayPerNamespace }

// RouteEnv is the route variables in render order; they reference $(AFS_GATEWAY_URL), so they must follow it.
// Nothing is returned for a team on the shared gateway.
func RouteEnv(t Team, r LLMRoutes) []domain.EnvVar {
	if !Delivered(t) {
		return nil
	}
	return domain.RouteEnv(r.Slugs(), r.DefaultRoute, r.DefaultModel, r.BedrockOn)
}

// RoutesJSON is /etc/afs/llm-routes.json with base URLs on gatewayURL (the file cannot expand variables).
func RoutesJSON(gatewayURL string, r LLMRoutes) string {
	type route struct {
		Route     string       `json:"route"`
		BaseURL   string       `json:"baseUrl"`
		Protocols []string     `json:"protocols"`
		Models    []RouteModel `json:"models"`
		Pricing   string       `json:"pricing"`
	}
	by := map[string]RouteSpec{}
	for _, rt := range r.Routes {
		by[rt.Slug] = rt
	}
	out := struct {
		Version      int     `json:"version"`
		DefaultRoute string  `json:"defaultRoute,omitempty"`
		DefaultModel string  `json:"defaultModel,omitempty"`
		Routes       []route `json:"routes"`
	}{Version: 1, DefaultRoute: r.DefaultRoute, DefaultModel: r.DefaultModel, Routes: []route{}}
	for _, s := range r.Slugs() {
		rt := by[s]
		models := slices.Clone(rt.Models)
		slices.SortFunc(models, func(a, b RouteModel) int { return strings.Compare(a.Name, b.Name) })
		if models == nil {
			models = []RouteModel{}
		}
		protocols := rt.Protocols
		if protocols == nil {
			protocols = []string{}
		}
		out.Routes = append(out.Routes, route{Route: s, BaseURL: strings.Replace(domain.RouteBaseURL(s), domain.GatewayURLRef, gatewayURL, 1),
			Protocols: protocols, Models: models, Pricing: rt.Pricing})
	}
	b, _ := json.Marshal(out) // plain strings and ints always marshal
	return string(b)
}

// RoutesHash is the pod-template hash line of the routes; empty for a team on the shared gateway with no
// route default, so the hash of a 1.1.0 agent does not change.
func RoutesHash(t Team, r LLMRoutes) string {
	if !Delivered(t) && r.DefaultRoute == "" {
		return ""
	}
	return "gateway=" + t.GatewayDeployment + " routes=" + RoutesJSON("", r) + " bedrock=" + map[bool]string{true: "on", false: "off"}[r.BedrockOn]
}
