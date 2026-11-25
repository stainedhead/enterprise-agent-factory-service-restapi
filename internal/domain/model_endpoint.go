package domain

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Model endpoints, routes and provider switches (API contract v1.5.0 §4.13, gateway contract §12,
// database contract §7.4, model endpoint gateway PRD §7).

// Error reasons of API contract v1.5.0 (§6 additions) and the gateway contract §12.
const (
	ReasonSlugTaken                  = "slug_taken"
	ReasonSlugReserved               = "slug_reserved"
	ReasonSlugInvalid                = "slug_invalid"
	ReasonBaseURLNotHTTPS            = "base_url_not_https"
	ReasonProviderDisabled           = "provider_disabled"
	ReasonNoFallbackModel            = "no_fallback_model"
	ReasonEndpointBaseURLPlaceholder = "endpoint_base_url_placeholder"
	ReasonNeedsServiceAdmin          = "needs_service_admin"
	ReasonTeamEndpointsNotAllowed    = "team_endpoints_not_allowed"
	ReasonEndpointEnabled            = "endpoint_enabled"
	ReasonEndpointCredentialPath     = "endpoint_credential_path" //nolint:gosec // an error reason, not a credential
	ReasonAgentRoutesTeam            = "agent_routes_team"
	ReasonAgentsDefaultRouteAllowed  = "agents_default_route_allowed"
	ReasonRouteNotEnabledForService  = "route_not_enabled_for_service"
	ReasonRouteNotEnabledForTeam     = "route_not_enabled_for_team"
	ReasonRouteNotEnabledForAgent    = "route_not_enabled_for_agent"
	ReasonModelNotOnRoute            = "model_not_on_route"
	ReasonUnsupportedParameter       = "unsupported_parameter"
	ReasonCredentialMissing          = "credential_missing" //nolint:gosec // an error reason, not a credential
	ReasonFederationNotReady         = "federation_not_ready"
	ReasonProviderBalanceExhausted   = "provider_balance_exhausted"
	ReasonProviderBudgetExhausted    = "provider_budget_exhausted"
)

// Provider keys seeded by migration 000023. ProviderInternal is not a row: corporate and in-namespace
// endpoints have no provider switch.
const (
	ProviderBedrock    = "bedrock"
	ProviderOpenAI     = "openai"
	ProviderOpenRouter = "openrouter"
	ProviderNous       = "nous"
	ProviderOpenCode   = "opencode"
	ProviderAzure      = "azure"
	ProviderGoogle     = "google"
	ProviderOther      = "other"
	ProviderInternal   = "internal"
)

// RouteBedrock is the built-in route name of Bedrock; no endpoint may use it.
const RouteBedrock = "bedrock"

// SharedCredentialScope is the credential scope of a shared service endpoint credential.
const SharedCredentialScope = "_shared"

// MaxEndpointModels is the most route models one endpoint lists.
const MaxEndpointModels = 500

// Protocol is an OpenAI-compatible protocol an endpoint serves.
type Protocol string

// Protocols.
const (
	ProtocolChat       Protocol = "openai_chat"
	ProtocolResponses  Protocol = "openai_responses"
	ProtocolEmbeddings Protocol = "openai_embeddings"
)

// EndpointLocation is where an endpoint runs.
type EndpointLocation string

// Locations.
const (
	LocationExternal    EndpointLocation = "external"
	LocationCorporate   EndpointLocation = "corporate"
	LocationInNamespace EndpointLocation = "in_namespace"
)

// AuthKind is how the gateway authenticates upstream.
type AuthKind string

// Auth kinds.
const (
	AuthNone            AuthKind = "none"
	AuthBearer          AuthKind = "bearer"
	AuthHeader          AuthKind = "header"
	AuthAzureFederated  AuthKind = "azure_federated"
	AuthGoogleFederated AuthKind = "google_federated"
)

// Federated reports whether the kind exchanges the gateway's identity instead of a stored credential.
func (k AuthKind) Federated() bool { return k == AuthAzureFederated || k == AuthGoogleFederated }

// NeedsCredential reports whether a stored credential is required.
func (k AuthKind) NeedsCredential() bool { return k == AuthBearer || k == AuthHeader }

// FederationKind returns azure or google for a federated kind, "" otherwise.
func (k AuthKind) FederationKind() string {
	switch k {
	case AuthAzureFederated:
		return "azure"
	case AuthGoogleFederated:
		return "google"
	}
	return ""
}

// PricingMode is how a route's calls are costed (PRD §9.1).
type PricingMode string

// Pricing modes.
const (
	PricingPerToken PricingMode = "per_token"
	PricingCapacity PricingMode = "capacity"
	PricingFree     PricingMode = "free"
)

// Capped reports whether calls reserve and settle against the team budget.
func (p PricingMode) Capped() bool { return p == PricingPerToken }

// CredentialMode is one credential for every team or one per team.
type CredentialMode string

// Credential modes.
const (
	CredentialShared  CredentialMode = "shared"
	CredentialPerTeam CredentialMode = "per_team"
)

// TeamSelection is who may opt a team in to a provider's endpoints.
type TeamSelection string

// Team selection rules.
const (
	SelectionTeamAdmin    TeamSelection = "team_admin"
	SelectionServiceAdmin TeamSelection = "service_admin"
)

// GatewayDeployment is the team's gateway: the shared one or a team gateway in its namespace.
type GatewayDeployment string

// Gateway deployments.
const (
	GatewayShared       GatewayDeployment = "shared"
	GatewayPerNamespace GatewayDeployment = "per_namespace"
)

var (
	routeSlugRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	providerKeyRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{1,30}$`)
	placeholderRE  = regexp.MustCompile(`<[A-Za-z_-]+>`)
	authHeaderRE   = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
	hostGlobRE     = regexp.MustCompile(`^(\*\.|\*-)?[a-z0-9.-]+$`)
	isEnum         = func(v string, vals ...string) bool { return slices.Contains(vals, v) }
	protocolValues = []string{string(ProtocolChat), string(ProtocolResponses), string(ProtocolEmbeddings)}
)

// ValidRouteSlug checks a route (endpoint) slug: the pattern and the reserved bedrock name.
func ValidRouteSlug(slug string) *FieldError {
	switch {
	case slug == RouteBedrock:
		return &FieldError{Field: "slug", Reason: ReasonSlugReserved}
	case !routeSlugRE.MatchString(slug):
		return &FieldError{Field: "slug", Reason: ReasonSlugInvalid}
	}
	return nil
}

// ValidProviderKey checks an inference provider key.
func ValidProviderKey(p string) bool { return providerKeyRE.MatchString(p) }

// RouteEnvName is the AFS_LLM_<ROUTE>_BASE_URL variable of a route (gateway contract §12.4).
func RouteEnvName(slug string) string {
	return "AFS_LLM_" + strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "_BASE_URL"
}

// ModelEndpoint is one configured upstream; its slug is the route name. It never holds a credential value.
type ModelEndpoint struct {
	ID              string
	TeamID          string // empty for a service endpoint
	Slug            string
	DisplayName     string
	Description     string
	Owner           string
	Purpose         string
	Provider        string
	Protocols       []Protocol
	Location        EndpointLocation
	BaseURL         string
	CABundle        string
	AuthKind        AuthKind
	AuthHeader      string
	MetricsPort     int
	CredentialMode  CredentialMode
	PricingMode     PricingMode
	RequiresTerms   bool
	UseProviderCost bool
	Enabled         bool
	TimeoutSeconds  int
	MaxBodyMiB      int
	ExternalSystem  string
	Builtin         bool
	Version         int
	CreatedAt       time.Time
	CreatedBy       string
	UpdatedAt       time.Time
}

// IsTeam reports whether the endpoint is declared by a team.
func (e ModelEndpoint) IsTeam() bool { return e.TeamID != "" }

// Serves reports whether the endpoint lists the protocol.
func (e ModelEndpoint) Serves(p Protocol) bool { return slices.Contains(e.Protocols, p) }

// Host returns the lower-case host of the base URL, "" when it does not parse.
func (e ModelEndpoint) Host() string {
	u, err := url.Parse(e.BaseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// HasPlaceholder reports whether the base URL still holds a built-in placeholder such as <resource>.
func (e ModelEndpoint) HasPlaceholder() bool { return placeholderRE.MatchString(e.BaseURL) }

// Validate checks the endpoint's own rules (database contract §7.4.2). Uniqueness of the slug and the
// provider switch are checked against stored state by the caller.
func (e ModelEndpoint) Validate() error {
	var fe []FieldError
	if f := ValidRouteSlug(e.Slug); f != nil {
		fe = append(fe, *f)
	}
	if strings.TrimSpace(e.DisplayName) == "" || len(e.DisplayName) > 200 {
		fe = append(fe, FieldError{Field: "displayName", Reason: "required, at most 200 characters"})
	}
	if len(e.Description) > 2000 {
		fe = append(fe, FieldError{Field: "description", Reason: "at most 2000 characters"})
	}
	if e.IsTeam() {
		if strings.TrimSpace(e.Owner) == "" || len(e.Owner) > 200 {
			fe = append(fe, FieldError{Field: "owner", Reason: "required on a team endpoint"})
		}
		if strings.TrimSpace(e.Purpose) == "" || len(e.Purpose) > 500 {
			fe = append(fe, FieldError{Field: "purpose", Reason: "required on a team endpoint"})
		}
		if e.CredentialMode != CredentialPerTeam {
			fe = append(fe, FieldError{Field: "credentialMode", Reason: "team endpoints are per_team"})
		}
	}
	if e.Provider != ProviderInternal && !ValidProviderKey(e.Provider) {
		fe = append(fe, FieldError{Field: "provider", Reason: "unknown provider"})
	}
	if e.Provider == ProviderBedrock {
		fe = append(fe, FieldError{Field: "provider", Reason: "bedrock is served by the built-in bedrock route"})
	}
	fe = append(fe, e.validateProtocols()...)
	fe = append(fe, e.validateLocation()...)
	fe = append(fe, e.validateAuth()...)
	if !isEnum(string(e.CredentialMode), string(CredentialShared), string(CredentialPerTeam)) {
		fe = append(fe, FieldError{Field: "credentialMode", Reason: "must be shared or per_team"})
	}
	if !isEnum(string(e.PricingMode), string(PricingPerToken), string(PricingCapacity), string(PricingFree)) {
		fe = append(fe, FieldError{Field: "pricingMode", Reason: "must be per_token, capacity or free"})
	}
	if e.TimeoutSeconds < 5 || e.TimeoutSeconds > 3600 {
		fe = append(fe, FieldError{Field: "timeoutSeconds", Reason: "must be 5 to 3600"})
	}
	if e.MaxBodyMiB < 1 || e.MaxBodyMiB > 100 {
		fe = append(fe, FieldError{Field: "maxBodyMib", Reason: "must be 1 to 100"})
	}
	if len(e.CABundle) > 16384 || (e.CABundle != "" && !strings.Contains(e.CABundle, "-----BEGIN CERTIFICATE-----")) {
		fe = append(fe, FieldError{Field: "caBundle", Reason: "must be PEM certificates, at most 16 KiB"})
	}
	if e.Enabled && e.HasPlaceholder() {
		fe = append(fe, FieldError{Field: "baseUrl", Reason: ReasonEndpointBaseURLPlaceholder})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

func (e ModelEndpoint) validateProtocols() []FieldError {
	if len(e.Protocols) == 0 {
		return []FieldError{{Field: "protocols", Reason: "required"}}
	}
	seen := map[Protocol]bool{}
	for _, p := range e.Protocols {
		if !isEnum(string(p), protocolValues...) || seen[p] {
			return []FieldError{{Field: "protocols", Reason: "a subset of openai_chat, openai_responses, openai_embeddings"}}
		}
		seen[p] = true
	}
	if !seen[ProtocolChat] {
		return []FieldError{{Field: "protocols", Reason: "must include openai_chat"}}
	}
	return nil
}

func (e ModelEndpoint) validateLocation() []FieldError {
	var fe []FieldError
	switch e.Location {
	case LocationExternal, LocationCorporate:
	case LocationInNamespace:
		if !e.IsTeam() {
			fe = append(fe, FieldError{Field: "location", Reason: "in_namespace is for team endpoints only"})
		}
	default:
		return []FieldError{{Field: "location", Reason: "must be external, corporate or in_namespace"}}
	}
	if len(e.BaseURL) == 0 || len(e.BaseURL) > 500 {
		return append(fe, FieldError{Field: "baseUrl", Reason: "required, at most 500 characters"})
	}
	u, err := url.Parse(e.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return append(fe, FieldError{Field: "baseUrl", Reason: "must be an absolute URL without credentials, query or fragment"})
	}
	switch {
	case e.Location != LocationInNamespace && u.Scheme != "https":
		fe = append(fe, FieldError{Field: "baseUrl", Reason: ReasonBaseURLNotHTTPS})
	case e.Location == LocationInNamespace && u.Scheme != "https" && u.Scheme != "http":
		fe = append(fe, FieldError{Field: "baseUrl", Reason: "must be http or https"})
	}
	if e.MetricsPort != 0 && (e.Location != LocationInNamespace || e.MetricsPort < 1 || e.MetricsPort > 65535) {
		fe = append(fe, FieldError{Field: "metricsPort", Reason: "in_namespace only, 1 to 65535"})
	}
	return fe
}

func (e ModelEndpoint) validateAuth() []FieldError {
	switch e.AuthKind {
	case AuthNone, AuthBearer, AuthAzureFederated, AuthGoogleFederated:
		if e.AuthHeader != "" {
			return []FieldError{{Field: "authHeader", Reason: "only with authKind header"}}
		}
	case AuthHeader:
		if !authHeaderRE.MatchString(e.AuthHeader) {
			return []FieldError{{Field: "authHeader", Reason: "required with authKind header"}}
		}
	default:
		return []FieldError{{Field: "authKind", Reason: "must be none, bearer, header, azure_federated or google_federated"}}
	}
	if e.AuthKind == AuthAzureFederated && e.Provider != ProviderAzure {
		return []FieldError{{Field: "authKind", Reason: "azure_federated needs provider azure"}}
	}
	if e.AuthKind == AuthGoogleFederated && e.Provider != ProviderGoogle {
		return []FieldError{{Field: "authKind", Reason: "google_federated needs provider google"}}
	}
	return nil
}

// EndpointModel is a model served through a route.
type EndpointModel struct {
	ModelName       string
	MaxOutputTokens int
	ContextTokens   int // 0 when unknown
	Enabled         bool
}

// ValidModelName checks a route model name: printable, at most 200 characters, not starting with / or a space.
func ValidModelName(n string) bool {
	if n == "" || len(n) > 200 || n[0] == '/' || n[0] == ' ' {
		return false
	}
	for _, r := range n {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// ValidateEndpointModels checks a full model list of an endpoint.
func ValidateEndpointModels(ms []EndpointModel, field string) error {
	var fe []FieldError
	if len(ms) > MaxEndpointModels {
		fe = append(fe, FieldError{Field: field, Reason: "at most 500 models"})
	}
	seen := map[string]bool{}
	for i, m := range ms {
		f := field + "[" + itoa(i) + "]"
		switch {
		case !ValidModelName(m.ModelName):
			fe = append(fe, FieldError{Field: f + ".modelName", Reason: "printable, at most 200 characters, not starting with /"})
		case seen[m.ModelName]:
			fe = append(fe, FieldError{Field: f + ".modelName", Reason: "is listed twice"})
		}
		seen[m.ModelName] = true
		if m.MaxOutputTokens < 1 || m.MaxOutputTokens > 1000000 {
			fe = append(fe, FieldError{Field: f + ".maxOutputTokens", Reason: "must be 1 to 1000000"})
		}
		if m.ContextTokens < 0 {
			fe = append(fe, FieldError{Field: f + ".contextTokens", Reason: "must be positive"})
		}
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// InferenceProvider is a provider switch (PRD §7.4), Bedrock included. Service-wide.
type InferenceProvider struct {
	Provider       string
	DisplayName    string
	Hosts          []string
	Enabled        bool
	TeamSelection  TeamSelection
	DisabledReason string
	FallbackModel  string
	Version        int
	ChangedBy      string
	ChangedAt      time.Time
}

// HostMatches reports whether host matches an FQDN glob: an exact name, "*.suffix" (one or more labels)
// or "*-suffix" (a label prefix, for example *-aiplatform.googleapis.com).
func HostMatches(glob, host string) bool {
	glob, host = strings.ToLower(glob), strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case strings.HasPrefix(glob, "*."):
		return strings.HasSuffix(host, glob[1:]) && len(host) > len(glob)-1
	case strings.HasPrefix(glob, "*-"):
		suffix := glob[1:]
		if !strings.HasSuffix(host, suffix) || len(host) == len(suffix) {
			return false
		}
		return !strings.Contains(host[:len(host)-len(suffix)], ".")
	case strings.Contains(glob, "*"):
		// bedrock-runtime.*.amazonaws.com: one label in place of the star.
		pre, post, _ := strings.Cut(glob, "*")
		if !strings.HasPrefix(host, pre) || !strings.HasSuffix(host, post) || len(host) <= len(pre)+len(post) {
			return false
		}
		return !strings.Contains(host[len(pre):len(host)-len(post)], ".")
	}
	return glob == host
}

// ValidHostGlob checks one provider host glob.
func ValidHostGlob(g string) bool {
	if strings.Count(g, "*") > 1 {
		return false
	}
	if strings.Contains(g, "*") && !strings.HasPrefix(g, "*") {
		pre, post, _ := strings.Cut(g, "*")
		return hostGlobRE.MatchString(strings.TrimSuffix(pre, ".")) && hostGlobRE.MatchString(strings.TrimPrefix(post, "."))
	}
	return hostGlobRE.MatchString(g)
}

// ProviderForHost returns the first provider (in the order given) whose hosts match, else other. It mirrors
// afs.provider_for_host of migration 000023.
func ProviderForHost(providers []InferenceProvider, host string) string {
	for _, p := range providers {
		for _, g := range p.Hosts {
			if HostMatches(g, host) {
				return p.Provider
			}
		}
	}
	return ProviderOther
}

// ProviderFor returns the provider of a team endpoint: internal for corporate and in-namespace locations,
// else the provider whose hosts match its base URL host.
func ProviderFor(providers []InferenceProvider, e ModelEndpoint) string {
	if e.Location == LocationCorporate || e.Location == LocationInNamespace {
		return ProviderInternal
	}
	return ProviderForHost(providers, e.Host())
}

// ParseRouteModel splits "<route>/<model>" into the route and model; ok is false when s is not of that form
// (a Bedrock model id never holds a slash before its first dot).
func ParseRouteModel(s string) (route, model string, ok bool) {
	route, model, found := strings.Cut(s, "/")
	if !found || !routeSlugRE.MatchString(route) || route == RouteBedrock || !ValidModelName(model) {
		return "", "", false
	}
	return route, model, true
}

// EndpointChangeEvents returns the event types of an endpoint update: enabled or disabled when the switch
// flips, updated when anything else changed, both when both did.
func EndpointChangeEvents(before, after ModelEndpoint, otherChanges bool) []EventType {
	var out []EventType
	if otherChanges {
		out = append(out, EventModelEndpointUpdated)
	}
	switch {
	case !before.Enabled && after.Enabled:
		out = append(out, EventModelEndpointEnabled)
	case before.Enabled && !after.Enabled:
		out = append(out, EventModelEndpointDisabled)
	}
	return out
}

// EndpointLifecycleEvent returns the event type of a create (true) or delete (false).
func EndpointLifecycleEvent(created bool) EventType {
	if created {
		return EventModelEndpointCreated
	}
	return EventModelEndpointDeleted
}

// EventEndpointCredentialSet is emitted on a credential write or rotation (no value in the event).
const EventEndpointCredentialSet = EventModelEndpointCredentialSet

// SelectionEvent returns model_endpoint.selected or .deselected.
func SelectionEvent(selected bool) EventType {
	if selected {
		return EventModelEndpointSelected
	}
	return EventModelEndpointDeselected
}

// ProviderSwitchEvent returns inference_provider.enabled or .disabled.
func ProviderSwitchEvent(enabled bool) EventType {
	if enabled {
		return EventInferenceProviderEnabled
	}
	return EventInferenceProviderDisabled
}

// RouteHealth is a route condition derived by the reconciler (PRD §10.4).
type RouteHealth string

// Route health states.
const (
	RouteHealthUnknown     RouteHealth = "Unknown"
	RouteHealthReady       RouteHealth = "Ready"
	RouteHealthDegraded    RouteHealth = "Degraded"
	RouteHealthUnavailable RouteHealth = "Unavailable"
)

// RouteHealthEvent returns the event of a health transition: degraded when a Ready (or Unknown) route
// becomes Degraded or Unavailable, recovered when it returns to Ready; ok is false otherwise.
func RouteHealthEvent(prev, next RouteHealth) (EventType, bool) {
	bad := func(h RouteHealth) bool { return h == RouteHealthDegraded || h == RouteHealthUnavailable }
	switch {
	case bad(next) && !bad(prev):
		return EventModelEndpointDegraded, true
	case next == RouteHealthReady && bad(prev):
		return EventModelEndpointRecovered, true
	}
	return "", false
}
