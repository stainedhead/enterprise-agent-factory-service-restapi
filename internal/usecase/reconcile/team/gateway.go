package team

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Team gateway (gateway contract §12.1, MEG-RC-1..6): when a team's gateway_deployment is per_namespace the
// reconciler renders afs-gateway in the team namespace, two replicas behind https://afs-gateway.<ns>.svc:8443,
// with egress to the team's route hosts only, the route credentials through an ExternalSecret and, for Azure
// and Google federation, a projected service-account token. Shared teams have none of it.

// Gateway deployments (teams.gateway_deployment).
const (
	DeploymentShared       = "shared"
	DeploymentPerNamespace = "per_namespace"
)

// The team gateway's fixed shape.
const (
	TeamGatewayName     = "afs-gateway"
	TeamGatewayPort     = 8443
	TeamGatewayReplicas = 2
	// AzureTokenAudience is the audience Entra ID workload identity federation accepts.
	AzureTokenAudience = "api://AzureADTokenExchange" //nolint:gosec // a token audience, not a credential
)

// TeamGatewayURL is AFS_GATEWAY_URL of a team on its own gateway.
func TeamGatewayURL(namespace string) string {
	return "https://" + TeamGatewayName + "." + namespace + ".svc:" + strconv.Itoa(TeamGatewayPort)
}

// Federation is a team's Azure or Google token exchange for one route (endpoint_federations).
type Federation struct {
	// Kind is azure or google.
	Kind string
	// TenantID and ClientID (azure); ProjectNumber, PoolID and ProviderID (google).
	TenantID, ClientID                string
	ProjectNumber, PoolID, ProviderID string
	// Status is pending, ready or failing.
	Status string
}

// GoogleAudience is the audience of the token a Google workload identity pool provider accepts.
func (f Federation) GoogleAudience() string {
	return "//iam.googleapis.com/projects/" + f.ProjectNumber + "/locations/global/workloadIdentityPools/" + f.PoolID + "/providers/" + f.ProviderID
}

// GatewayEndpoint is one route in the team's set (enabled, on a provider that is on, the team's own or opted in).
type GatewayEndpoint struct {
	ID, Slug string
	// Location is external, corporate or in_namespace.
	Location string
	BaseURL  string
	// AuthKind is none, bearer, header, azure_federated or google_federated.
	AuthKind string
	// CredentialARN is the Secrets Manager reference the team's calls use (per-team, else shared); empty when
	// none is stored. CredentialMissing is the reconciler's own view (endpoint_credentials.status).
	CredentialARN     string
	CredentialMissing bool
	Federation        *Federation
}

// needsCredential reports whether calls on the route carry a stored credential.
func (e GatewayEndpoint) needsCredential() bool {
	return e.AuthKind == "bearer" || e.AuthKind == "header"
}

// TeamGateway is the gateway part of a team.
type TeamGateway struct {
	// Deployment is shared or per_namespace.
	Deployment string
	Endpoints  []GatewayEndpoint
	// BedrockOn is the Bedrock provider switch: off, the team gateway has no Bedrock egress.
	BedrockOn bool
	// Mcp is the team's MCP servers (v1.3.0).
	Mcp McpGateway
}

// PerNamespace reports whether the team runs its own gateway.
func (g TeamGateway) PerNamespace() bool { return g.Deployment == DeploymentPerNamespace }

// EgressRule is one destination the team gateway may reach.
type EgressRule struct {
	// Host is a DNS name (FQDN rule); InNamespace rules reach pods of the team namespace on Port instead.
	Host        string
	Port        int
	InNamespace bool
}

// hostPort splits a base URL; the port defaults by scheme.
func hostPort(raw string) (string, int, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", 0, false
	}
	p := 443
	if u.Scheme == "http" {
		p = 80
	}
	if u.Port() != "" {
		n, err := strconv.Atoi(u.Port())
		if err != nil {
			return "", 0, false
		}
		p = n
	}
	return strings.ToLower(u.Hostname()), p, true
}

// Egress lists the destinations of the team gateway, sorted and unique: each route's host (in-namespace routes
// by port within the namespace), Bedrock runtime while Bedrock is on, the Entra ID token endpoint for Azure
// federation and Google STS and IAM credentials for Google federation.
func (g TeamGateway) Egress(region string) []EgressRule {
	seen := map[EgressRule]bool{}
	add := func(r EgressRule) { seen[r] = true }
	for _, e := range g.Endpoints {
		h, p, ok := hostPort(e.BaseURL)
		if !ok {
			continue
		}
		if e.Location == "in_namespace" {
			add(EgressRule{Port: p, InNamespace: true})
		} else {
			add(EgressRule{Host: h, Port: p})
		}
		if e.Federation != nil {
			switch e.Federation.Kind {
			case "azure":
				add(EgressRule{Host: "login.microsoftonline.com", Port: 443})
			case "google":
				add(EgressRule{Host: "sts.googleapis.com", Port: 443})
				add(EgressRule{Host: "iamcredentials.googleapis.com", Port: 443})
			}
		}
	}
	if g.BedrockOn && region != "" {
		add(EgressRule{Host: "bedrock-runtime." + region + ".amazonaws.com", Port: 443})
	}
	for _, r := range g.Mcp.mcpEgress() {
		add(r)
	}
	out := make([]EgressRule, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b EgressRule) int {
		if c := strings.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		return a.Port - b.Port
	})
	return out
}

// Credential is one key of the team gateway's credentials Secret.
type Credential struct {
	// Key is the route slug.
	Key, SecretARN string
}

// Credentials lists the route credentials the team gateway receives, by slug, and the MCP server credentials as
// mcp.<slug>. Only references are listed.
func (g TeamGateway) Credentials() []Credential {
	var out []Credential
	for _, e := range g.Endpoints {
		if e.needsCredential() && e.CredentialARN != "" {
			out = append(out, Credential{Key: e.Slug, SecretARN: e.CredentialARN})
		}
	}
	out = append(out, g.Mcp.mcpCredentials()...)
	slices.SortFunc(out, func(a, b Credential) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// Federated reports whether a route of the kind (azure or google) is federated for the team.
func (g TeamGateway) Federated(kind string) bool {
	return slices.ContainsFunc(g.Endpoints, func(e GatewayEndpoint) bool { return e.Federation != nil && e.Federation.Kind == kind })
}

// GoogleAudiences lists the distinct audiences of the team's Google federations, sorted.
func (g TeamGateway) GoogleAudiences() []string {
	var out []string
	for _, e := range g.Endpoints {
		if e.Federation != nil && e.Federation.Kind == "google" {
			out = append(out, e.Federation.GoogleAudience())
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Route health reasons (RouteHealth condition, PRD §10.4).
const (
	ReasonRouteReady         = "Ready"
	ReasonGatewayShared      = "team_gateway_not_deployed"
	ReasonCredentialMissing  = "credential_missing" //nolint:gosec // a reason code
	ReasonFederationNotReady = "federation_not_ready"
	ReasonFederationPending  = "federation_pending"
)

// RouteStatus is the health of one route for one team.
type RouteStatus struct {
	EndpointID, Slug string
	Health           domain.RouteHealth
	Reason           string
}

// RouteHealth derives each route's health from what the reconciler knows: a shared team has no gateway that
// serves routes; a route that needs a credential and has none, or whose federation is failing or absent, is
// Unavailable; a pending federation is Degraded; anything else is Ready.
func (g TeamGateway) RouteHealth() []RouteStatus {
	out := make([]RouteStatus, 0, len(g.Endpoints))
	for _, e := range g.Endpoints {
		s := RouteStatus{EndpointID: e.ID, Slug: e.Slug, Health: domain.RouteHealthReady, Reason: ReasonRouteReady}
		switch {
		case !g.PerNamespace():
			s.Health, s.Reason = domain.RouteHealthUnavailable, ReasonGatewayShared
		case e.needsCredential() && (e.CredentialARN == "" || e.CredentialMissing):
			s.Health, s.Reason = domain.RouteHealthUnavailable, ReasonCredentialMissing
		case e.AuthKind == "azure_federated" || e.AuthKind == "google_federated":
			switch {
			case e.Federation == nil || e.Federation.Status == "failing":
				s.Health, s.Reason = domain.RouteHealthUnavailable, ReasonFederationNotReady
			case e.Federation.Status == "pending":
				s.Health, s.Reason = domain.RouteHealthDegraded, ReasonFederationPending
			}
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b RouteStatus) int { return strings.Compare(a.Slug, b.Slug) })
	return out
}

// RouteReporter records the route health of a team (resource_status kinds team_route and team_gateway) and
// publishes model_endpoint.degraded or .recovered on a transition.
type RouteReporter interface {
	ReportRoutes(ctx context.Context, teamID string, gatewayReady bool, routes []RouteStatus) error
}
