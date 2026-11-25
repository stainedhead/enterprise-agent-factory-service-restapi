package domain

import (
	"strings"
	"testing"
)

func validEndpoint() ModelEndpoint {
	return ModelEndpoint{Slug: "openai-prod", DisplayName: "OpenAI", Provider: ProviderOpenAI,
		Protocols: []Protocol{ProtocolChat, ProtocolResponses}, Location: LocationExternal,
		BaseURL: "https://api.openai.com/v1", AuthKind: AuthBearer, CredentialMode: CredentialPerTeam,
		PricingMode: PricingPerToken, TimeoutSeconds: 300, MaxBodyMiB: 20}
}

func reasons(err error) string {
	var out []string
	for _, f := range FieldErrors(err) {
		out = append(out, f.Field+":"+f.Reason)
	}
	return strings.Join(out, ";")
}

func TestValidRouteSlug(t *testing.T) {
	for slug, want := range map[string]string{"openai-prod": "", "bedrock": ReasonSlugReserved, "A": ReasonSlugInvalid,
		"x": ReasonSlugInvalid, "1abc": ReasonSlugInvalid, strings.Repeat("a", 32): ReasonSlugInvalid} {
		got := ValidRouteSlug(slug)
		if (want == "") != (got == nil) || (got != nil && got.Reason != want) {
			t.Errorf("%s = %v, want %q", slug, got, want)
		}
	}
	if RouteEnvName("openai-prod") != "AFS_LLM_OPENAI_PROD_BASE_URL" || RouteEnvName("bedrock") != "AFS_LLM_BEDROCK_BASE_URL" {
		t.Error("RouteEnvName")
	}
}

func TestModelEndpointValidate(t *testing.T) {
	if err := validEndpoint().Validate(); err != nil {
		t.Fatalf("valid endpoint: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*ModelEndpoint)
		want string
	}{
		{"http external", func(e *ModelEndpoint) { e.BaseURL = "http://api.openai.com/v1" }, ReasonBaseURLNotHTTPS},
		{"bad url", func(e *ModelEndpoint) { e.BaseURL = "https://u:p@x.com/v1" }, "without credentials"},
		{"empty url", func(e *ModelEndpoint) { e.BaseURL = "" }, "baseUrl:required"},
		{"placeholder on", func(e *ModelEndpoint) {
			e.BaseURL = "https://<resource>.openai.azure.com/openai/v1"
			e.Enabled = true
		}, ReasonEndpointBaseURLPlaceholder},
		{"no chat", func(e *ModelEndpoint) { e.Protocols = []Protocol{ProtocolResponses} }, "must include openai_chat"},
		{"no protocols", func(e *ModelEndpoint) { e.Protocols = nil }, "protocols:required"},
		{"dup protocol", func(e *ModelEndpoint) { e.Protocols = []Protocol{ProtocolChat, ProtocolChat} }, "a subset"},
		{"in_namespace service", func(e *ModelEndpoint) { e.Location = LocationInNamespace }, "team endpoints only"},
		{"bad location", func(e *ModelEndpoint) { e.Location = "moon" }, "location:must be"},
		{"metrics external", func(e *ModelEndpoint) { e.MetricsPort = 9000 }, "metricsPort"},
		{"header without name", func(e *ModelEndpoint) { e.AuthKind = AuthHeader }, "authHeader:required"},
		{"header name on bearer", func(e *ModelEndpoint) { e.AuthHeader = "X-Key" }, "only with authKind header"},
		{"bad auth", func(e *ModelEndpoint) { e.AuthKind = "magic" }, "authKind:must be"},
		{"azure fed on openai", func(e *ModelEndpoint) { e.AuthKind = AuthAzureFederated }, "needs provider azure"},
		{"google fed on openai", func(e *ModelEndpoint) { e.AuthKind = AuthGoogleFederated }, "needs provider google"},
		{"bedrock provider", func(e *ModelEndpoint) { e.Provider = ProviderBedrock }, "built-in bedrock route"},
		{"bad provider", func(e *ModelEndpoint) { e.Provider = "Bad!" }, "unknown provider"},
		{"bad cred mode", func(e *ModelEndpoint) { e.CredentialMode = "x" }, "credentialMode"},
		{"bad pricing", func(e *ModelEndpoint) { e.PricingMode = "x" }, "pricingMode"},
		{"timeout", func(e *ModelEndpoint) { e.TimeoutSeconds = 1 }, "timeoutSeconds"},
		{"body", func(e *ModelEndpoint) { e.MaxBodyMiB = 101 }, "maxBodyMib"},
		{"ca", func(e *ModelEndpoint) { e.CABundle = "nope" }, "caBundle"},
		{"name", func(e *ModelEndpoint) { e.DisplayName = " " }, "displayName"},
		{"description", func(e *ModelEndpoint) { e.Description = strings.Repeat("x", 1001) }, "description"},
		{"slug", func(e *ModelEndpoint) { e.Slug = "bedrock" }, ReasonSlugReserved},
		{"team without owner", func(e *ModelEndpoint) { e.TeamID = "team_1" }, "owner:required"},
		{"team shared", func(e *ModelEndpoint) {
			e.TeamID, e.Owner, e.Purpose, e.CredentialMode = "team_1", "o", "p", CredentialShared
		}, "team endpoints are per_team"},
	}
	for _, c := range cases {
		e := validEndpoint()
		c.mut(&e)
		if got := reasons(e.Validate()); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	team := validEndpoint()
	team.TeamID, team.Owner, team.Purpose, team.Provider = "team_1", "platform", "coding", ProviderInternal
	team.Location, team.BaseURL, team.AuthKind, team.MetricsPort = LocationInNamespace, "http://vllm.afs-t-a.svc:8000/v1", AuthNone, 8000
	if err := team.Validate(); err != nil {
		t.Errorf("in-namespace team endpoint: %v", err)
	}
	team.BaseURL = "ftp://x/v1"
	if !strings.Contains(reasons(team.Validate()), "http or https") {
		t.Error("in-namespace scheme")
	}
}

func TestEndpointHelpers(t *testing.T) {
	e := validEndpoint()
	if !e.Serves(ProtocolResponses) || e.Serves(ProtocolEmbeddings) || e.IsTeam() || e.Host() != "api.openai.com" || e.HasPlaceholder() {
		t.Error("helpers")
	}
	e.BaseURL = "://bad"
	if e.Host() != "" {
		t.Error("bad host")
	}
	if !AuthAzureFederated.Federated() || AuthBearer.Federated() || !AuthHeader.NeedsCredential() || AuthNone.NeedsCredential() {
		t.Error("auth kinds")
	}
	if AuthAzureFederated.FederationKind() != "azure" || AuthGoogleFederated.FederationKind() != "google" || AuthBearer.FederationKind() != "" {
		t.Error("federation kind")
	}
	if !PricingPerToken.Capped() || PricingCapacity.Capped() {
		t.Error("capped")
	}
}

func TestEndpointModels(t *testing.T) {
	if !ValidModelName("gpt-4.1") || !ValidModelName("google/gemini-2.5-pro") || ValidModelName("/x") || ValidModelName("") ||
		ValidModelName(" x") || ValidModelName("a\tb") || ValidModelName("é") || ValidModelName(strings.Repeat("a", 201)) {
		t.Error("ValidModelName")
	}
	ok := []EndpointModel{{ModelName: "a", MaxOutputTokens: 10}, {ModelName: "b", MaxOutputTokens: 1, ContextTokens: 5}}
	if err := ValidateEndpointModels(ok, "models"); err != nil {
		t.Fatal(err)
	}
	bad := []EndpointModel{{ModelName: "a", MaxOutputTokens: 0}, {ModelName: "a", MaxOutputTokens: 1, ContextTokens: -1}, {ModelName: "/x", MaxOutputTokens: 1}}
	got := reasons(ValidateEndpointModels(bad, "models"))
	for _, w := range []string{"models[0].maxOutputTokens", "models[1].modelName:is listed twice", "models[1].contextTokens", "models[2].modelName"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in %s", w, got)
		}
	}
	many := make([]EndpointModel, 501)
	for i := range many {
		many[i] = EndpointModel{ModelName: "m" + itoa(i), MaxOutputTokens: 1}
	}
	if !strings.Contains(reasons(ValidateEndpointModels(many, "models")), "at most 500") {
		t.Error("limit")
	}
}

func TestHostsAndProviders(t *testing.T) {
	cases := []struct {
		glob, host string
		want       bool
	}{
		{"api.openai.com", "API.openai.com", true},
		{"api.openai.com", "x.openai.com", false},
		{"*.openai.azure.com", "res.openai.azure.com", true},
		{"*.openai.azure.com", "a.b.openai.azure.com", true},
		{"*.openai.azure.com", "openai.azure.com", false},
		{"*-aiplatform.googleapis.com", "us-central1-aiplatform.googleapis.com", true},
		{"*-aiplatform.googleapis.com", "a.b-aiplatform.googleapis.com", false},
		{"*-aiplatform.googleapis.com", "-aiplatform.googleapis.com", false},
		{"*-aiplatform.googleapis.com", "aiplatform.googleapis.com", false},
		{"bedrock-runtime.*.amazonaws.com", "bedrock-runtime.us-east-1.amazonaws.com", true},
		{"bedrock-runtime.*.amazonaws.com", "bedrock-runtime.a.b.amazonaws.com", false},
		{"bedrock-runtime.*.amazonaws.com", "bedrock-runtime..amazonaws.com", false},
	}
	for _, c := range cases {
		if HostMatches(c.glob, c.host) != c.want {
			t.Errorf("HostMatches(%s, %s) != %v", c.glob, c.host, c.want)
		}
	}
	for g, want := range map[string]bool{"api.openai.com": true, "*.openai.azure.com": true, "*-aiplatform.googleapis.com": true,
		"bedrock-runtime.*.amazonaws.com": true, "**.x": false, "a*b*c": false, "UPPER.com": false} {
		if ValidHostGlob(g) != want {
			t.Errorf("ValidHostGlob(%s) != %v", g, want)
		}
	}
	ps := []InferenceProvider{{Provider: ProviderOpenAI, Hosts: []string{"api.openai.com"}},
		{Provider: ProviderAzure, Hosts: []string{"*.openai.azure.com"}}, {Provider: ProviderOther}}
	if ProviderForHost(ps, "res.openai.azure.com") != ProviderAzure || ProviderForHost(ps, "llm.example.com") != ProviderOther {
		t.Error("ProviderForHost")
	}
	e := validEndpoint()
	if ProviderFor(ps, e) != ProviderOpenAI {
		t.Error("ProviderFor external")
	}
	e.Location = LocationCorporate
	if ProviderFor(ps, e) != ProviderInternal {
		t.Error("ProviderFor corporate")
	}
	if !ValidProviderKey("openrouter") || ValidProviderKey("X") {
		t.Error("ValidProviderKey")
	}
}

func TestParseRouteModel(t *testing.T) {
	if r, m, ok := ParseRouteModel("azure-openai/gpt-5.1"); !ok || r != "azure-openai" || m != "gpt-5.1" {
		t.Error("route model")
	}
	if r, m, ok := ParseRouteModel("vertex-ai/google/gemini-2.5-pro"); !ok || r != "vertex-ai" || m != "google/gemini-2.5-pro" {
		t.Error("model with slash")
	}
	for _, s := range []string{"anthropic.claude-sonnet-4-5", "bedrock/x", "Bad/x", "a1/", "ok-route//x"} {
		if _, _, ok := ParseRouteModel(s); ok {
			t.Errorf("%s parsed", s)
		}
	}
}

func TestModelEndpointEvents(t *testing.T) {
	off, on := validEndpoint(), validEndpoint()
	on.Enabled = true
	if got := EndpointChangeEvents(off, on, false); len(got) != 1 || got[0] != EventModelEndpointEnabled {
		t.Errorf("enable = %v", got)
	}
	if got := EndpointChangeEvents(on, off, true); len(got) != 2 || got[0] != EventModelEndpointUpdated || got[1] != EventModelEndpointDisabled {
		t.Errorf("disable+update = %v", got)
	}
	if len(EndpointChangeEvents(on, on, false)) != 0 {
		t.Error("no change")
	}
	if EndpointLifecycleEvent(true) != EventModelEndpointCreated || EndpointLifecycleEvent(false) != EventModelEndpointDeleted {
		t.Error("lifecycle")
	}
	if SelectionEvent(true) != EventModelEndpointSelected || SelectionEvent(false) != EventModelEndpointDeselected {
		t.Error("selection")
	}
	if ProviderSwitchEvent(true) != EventInferenceProviderEnabled || ProviderSwitchEvent(false) != EventInferenceProviderDisabled {
		t.Error("provider")
	}
	if EventEndpointCredentialSet != EventModelEndpointCredentialSet {
		t.Error("credential")
	}
	for _, c := range []struct {
		prev, next RouteHealth
		want       EventType
		ok         bool
	}{
		{RouteHealthReady, RouteHealthDegraded, EventModelEndpointDegraded, true},
		{RouteHealthUnknown, RouteHealthUnavailable, EventModelEndpointDegraded, true},
		{RouteHealthDegraded, RouteHealthUnavailable, "", false},
		{RouteHealthUnavailable, RouteHealthReady, EventModelEndpointRecovered, true},
		{RouteHealthUnknown, RouteHealthReady, "", false},
	} {
		if got, ok := RouteHealthEvent(c.prev, c.next); got != c.want || ok != c.ok {
			t.Errorf("%s→%s = %s %v", c.prev, c.next, got, ok)
		}
	}
}
