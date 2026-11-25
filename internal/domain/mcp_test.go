package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

const mcpTestImage = "registry.example.com/mcp/github@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func remoteServer() McpServer {
	return McpServer{
		ID: "mcp_01", Slug: "github", DisplayName: "GitHub", DataClassification: "internal",
		Transport: McpTransportStreamableHTTP, Location: LocationExternal, URL: "https://api.githubcopilot.com/mcp/",
		AuthKind: McpAuthOAuthClient, ReviewStatus: McpDraft,
	}
}

func hostedServer() McpServer {
	return McpServer{
		ID: "mcp_02", TeamID: "team_01", Slug: "jira-local", DisplayName: "Jira", Owner: "platform", Purpose: "tickets",
		DataClassification: "confidential", Transport: McpTransportHosted, Location: LocationInNamespace, AuthKind: McpAuthStatic,
		Hosted: &McpHosted{Image: mcpTestImage, EnvNames: []string{"JIRA_TOKEN"}, InstanceMode: "per_team", EgressHosts: []string{"*.atlassian.net"}},
	}
}

func mcpReasons(err error) string {
	var e *Error
	if !errors.As(err, &e) || e == nil {
		return ""
	}
	var out []string
	for _, f := range e.Fields {
		out = append(out, f.Field+"="+f.Reason)
	}
	if e.Reason != "" {
		out = append(out, e.Reason)
	}
	return strings.Join(out, ";")
}

func TestValidMcpSlug(t *testing.T) {
	for slug, want := range map[string]string{
		"github": "", "jira-cloud": "", "afs": ReasonSlugReserved, "a": ReasonSlugInvalid, "Git": ReasonSlugInvalid,
		"a--b": ReasonSlugInvalid, "ab-": ReasonSlugInvalid, "1ab": ReasonSlugInvalid, strings.Repeat("a", 32): ReasonSlugInvalid,
	} {
		got := ""
		if f := ValidMcpSlug(slug); f != nil {
			got = f.Reason
		}
		if got != want {
			t.Errorf("%q = %q, want %q", slug, got, want)
		}
	}
}

func TestMcpServerValidate(t *testing.T) {
	fed := &McpFederation{Issuer: "https://oidc.eks.example", Audience: "api://x", TokenEndpoint: "https://login.example/token"} //nolint:gosec // a test URL, not a credential
	cases := []struct {
		name string
		mod  func(*McpServer)
		want string // substring of the reasons; "" means valid
	}{
		{"remote ok", func(*McpServer) {}, ""},
		{"hosted ok", nil, ""},
		{"http url", func(s *McpServer) { s.URL = "http://x.example/mcp" }, "url=" + ReasonURLNotHTTPS},
		{"url with user", func(s *McpServer) { s.URL = "https://u:p@x.example/mcp" }, "url=" + ReasonURLNotHTTPS},
		{"remote in namespace", func(s *McpServer) { s.Location = LocationInNamespace }, "location="},
		{"remote with hosted", func(s *McpServer) { s.Hosted = &McpHosted{} }, "hosted=only"},
		{"bad transport", func(s *McpServer) { s.Transport = "stdio" }, "transport="},
		{"display name", func(s *McpServer) { s.DisplayName = " " }, "displayName="},
		{"doc url", func(s *McpServer) { s.DocURL = "http://docs" }, "docUrl=" + ReasonURLNotHTTPS},
		{"classification", func(s *McpServer) { s.DataClassification = "secret" }, "dataClassification="},
		{"auth unknown", func(s *McpServer) { s.AuthKind = "kerberos" }, "authKind=" + ReasonAuthKindUnsupported},
		{"federation missing", func(s *McpServer) { s.AuthKind = McpAuthWorkloadFederation }, "federation=issuer"},
		{"federation http", func(s *McpServer) {
			s.AuthKind, s.Federation = McpAuthWorkloadFederation, &McpFederation{Issuer: "i", Audience: "a", TokenEndpoint: "http://t"}
		}, "federation.tokenEndpoint="},
		{"federation ok", func(s *McpServer) { s.AuthKind, s.Federation = McpAuthWorkloadFederation, fed }, ""},
		{"federation on static", func(s *McpServer) { s.AuthKind, s.Federation = McpAuthStatic, fed }, "federation=only"},
		{"auth header", func(s *McpServer) { s.AuthHeader = "X Bad" }, "authHeader="},
		{"sigv4 ok", func(s *McpServer) { s.AuthKind = McpAuthAWSSigV4 }, ""},
		{"default unpublished", func(s *McpServer) { s.DefaultForNewTeams = true }, ReasonServerNotPublished},
	}
	for _, c := range cases {
		s := remoteServer()
		if c.mod == nil {
			s = hostedServer()
		} else {
			c.mod(&s)
		}
		got := mcpReasons(s.Validate())
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	hosted := []struct {
		name string
		mod  func(*McpServer)
		want string
	}{
		{"not pinned", func(s *McpServer) { s.Hosted.Image = "registry.example.com/mcp/github:latest" }, ReasonImageNotPinned},
		{"no spec", func(s *McpServer) { s.Hosted = nil }, "hosted=required"},
		{"external", func(s *McpServer) { s.Location = LocationExternal }, "location=a hosted"},
		{"url", func(s *McpServer) { s.URL = "https://x" }, "url=a hosted"},
		{"env", func(s *McpServer) { s.Hosted.EnvNames = []string{"lower"} }, "hosted.envNames"},
		{"args", func(s *McpServer) { s.Hosted.Args = make([]string, 65) }, "hosted.args"},
		{"instance", func(s *McpServer) { s.Hosted.InstanceMode = "per_pod" }, "hosted.instanceMode"},
		{"egress count", func(s *McpServer) { s.Hosted.EgressHosts = make([]string, 51) }, "hosted.egressHosts=at most"},
		{"egress glob", func(s *McpServer) { s.Hosted.EgressHosts = []string{"https://x"} }, "hosted.egressHosts=must"},
		{"team owner", func(s *McpServer) { s.Owner = "" }, "owner="},
		{"team purpose", func(s *McpServer) { s.Purpose = "" }, "purpose="},
		{"team published", func(s *McpServer) { s.Published = true }, "published="},
		{"sigv4 in namespace", func(s *McpServer) { s.AuthKind = McpAuthAWSSigV4 }, "authKind="},
	}
	for _, c := range hosted {
		s := hostedServer()
		h := *s.Hosted
		s.Hosted = &h
		c.mod(&s)
		if got := mcpReasons(s.Validate()); !strings.Contains(got, c.want) {
			t.Errorf("hosted %s: %q, want %q", c.name, got, c.want)
		}
	}
	if !remoteServer().AuthKind.NeedsCredential() || McpAuthNone.NeedsCredential() {
		t.Error("NeedsCredential")
	}
	if h := remoteServer().Host(); h != "api.githubcopilot.com" {
		t.Errorf("host = %q", h)
	}
	if h := (McpServer{URL: "://"}).Host(); h != "" {
		t.Errorf("bad url host = %q", h)
	}
	if !hostedServer().IsTeam() || remoteServer().IsTeam() {
		t.Error("IsTeam")
	}
}

func TestImageRegistryAndHosts(t *testing.T) {
	for img, want := range map[string]string{
		mcpTestImage: "registry.example.com", "localhost/x@sha256:1": "localhost", "library/x@sha256:1": "",
		"x@sha256:1": "", "reg:5000/x@sha256:1": "reg:5000",
	} {
		if got := ImageRegistry(img); got != want {
			t.Errorf("ImageRegistry(%s) = %q, want %q", img, got, want)
		}
	}
	if !RegistryAllowed(mcpTestImage, []string{"REGISTRY.example.com"}) || RegistryAllowed(mcpTestImage, []string{"other"}) ||
		RegistryAllowed("x@sha256:1", []string{""}) {
		t.Error("RegistryAllowed")
	}
	if !ValidMcpEnvNames(nil) || ValidMcpEnvNames(make([]string, 65)) {
		t.Error("ValidMcpEnvNames")
	}
}

func TestReviewTransitions(t *testing.T) {
	allowed := map[[2]McpReviewStatus]bool{
		{McpDraft, McpPendingReview}: true, {McpPendingReview, McpApproved}: true, {McpPendingReview, McpRejected}: true,
		{McpPendingReview, McpDraft}: true, {McpRejected, McpDraft}: true, {McpApproved, McpPendingReview}: true,
	}
	all := []McpReviewStatus{McpDraft, McpPendingReview, McpApproved, McpRejected}
	for _, from := range all {
		for _, to := range all {
			want := from == to || allowed[[2]McpReviewStatus{from, to}]
			if got := ReviewTransitionAllowed(from, to, ""); got != want {
				t.Errorf("%s → %s = %v, want %v", from, to, got, want)
			}
		}
		if !ReviewTransitionAllowed(from, McpSuspended, "") {
			t.Errorf("%s → suspended refused", from)
		}
		if !ReviewTransitionAllowed(McpSuspended, from, from) {
			t.Errorf("resume to %s refused", from)
		}
	}
	if ReviewTransitionAllowed(McpSuspended, McpApproved, McpDraft) || ReviewTransitionAllowed("bogus", McpDraft, "") {
		t.Error("resume to another status, or an unknown status, allowed")
	}
	a, b := hostedServer(), hostedServer()
	if ReviewChangingUpdate(a, b) {
		t.Error("no change counted")
	}
	b.Hosted = &McpHosted{Image: mcpTestImage + "x"}
	if !ReviewChangingUpdate(a, b) {
		t.Error("image change not counted")
	}
	c := remoteServer()
	d := c
	d.Federation = &McpFederation{Issuer: "i"}
	if !ReviewChangingUpdate(c, d) {
		t.Error("federation change not counted")
	}
}

// TestTeamServerApproval is the MCP-RA-1 table test over the three policy values (and an unknown one).
func TestTeamServerApproval(t *testing.T) {
	tiers := []string{"low", "elevated", "high"}
	want := map[string][3]bool{
		McpApprovalTeamAll:               {true, true, true},
		McpApprovalTeamLowSecurityHigher: {true, false, false},
		McpApprovalSecurityAll:           {false, false, false},
		"bogus":                          {false, false, false},
	}
	for policy, w := range want {
		for i, tier := range tiers {
			if got := TeamAdminMayApprove(policy, tier); got != w[i] {
				t.Errorf("%s/%s = %v, want %v", policy, tier, got, w[i])
			}
		}
	}
	if TeamAdminMayApprove(McpApprovalTeamAll, "unknown") {
		t.Error("an unknown tier must go to security")
	}
}

func TestMcpPolicyRuleConversion(t *testing.T) {
	n := 30
	r := McpPolicyRule{Name: "delete_*", Effect: "block", RateLimitPerMinute: &n}.Rule()
	if !r.Valid() || r.RateLimit != 30 || r.Effect != mcp.EffectBlock {
		t.Errorf("rule = %+v", r)
	}
	if (McpPolicyRule{Class: mcp.ClassReadOnly, Effect: "allow"}).Rule().RateLimit != 0 {
		t.Error("no rate limit")
	}
}

func TestMcpPolicyRules(t *testing.T) {
	one := 1
	zero := 0
	cases := []struct {
		name  string
		rules []McpPolicyRule
		want  string
	}{
		{"ok", []McpPolicyRule{{Name: "delete_*", Effect: "block"}, {Class: mcp.ClassReadOnly, Effect: "allow", RateLimitPerMinute: &one}}, ""},
		{"both", []McpPolicyRule{{Name: "x", Class: mcp.ClassDestructive, Effect: "block"}}, "exactly one"},
		{"neither", []McpPolicyRule{{Effect: "block"}}, "exactly one"},
		{"class", []McpPolicyRule{{Class: "risky", Effect: "block"}}, "class must"},
		{"effect", []McpPolicyRule{{Name: "x", Effect: "deny"}}, "effect must"},
		{"rate", []McpPolicyRule{{Name: "x", Effect: "allow", RateLimitPerMinute: &zero}}, "rateLimit"},
		{"long", []McpPolicyRule{{Name: strings.Repeat("x", 501), Effect: "allow"}}, "at most 500 characters"},
		{"dup", []McpPolicyRule{{Name: "x", Effect: "allow"}, {Name: "x", Effect: "block"}}, "duplicate"},
		{"count", make([]McpPolicyRule, 501), "at most 500 rules"},
	}
	for _, c := range cases {
		got := mcpReasons(ValidateMcpPolicyRules(c.rules, "rules"))
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
		if c.want != "" && !strings.Contains(got, ReasonInvalidPolicyRule) {
			t.Errorf("%s lacks the reason code: %q", c.name, got)
		}
	}
}

func TestAgentMcp(t *testing.T) {
	team := []string{"mcp_a", "mcp_b"}
	cases := []struct {
		name string
		a    AgentMcp
		want string
	}{
		{"all", AgentMcp{Access: "all", Mode: McpAgentModeSearch}, ""},
		{"selected", AgentMcp{Access: "selected", ServerIDs: []string{"mcp_a"}, Blocks: []McpAgentBlock{{ServerID: "mcp_a", Name: "delete_*", Effect: "block"}}}, ""},
		{"access", AgentMcp{Access: "some"}, "mcp.access"},
		{"ids with all", AgentMcp{Access: "all", ServerIDs: []string{"mcp_a"}}, "only with access selected"},
		{"mode", AgentMcp{Access: "all", Mode: "auto"}, "mcp.mode"},
		{"not on team", AgentMcp{Access: "selected", ServerIDs: []string{"mcp_z"}}, ReasonMcpServerNotEnabledForTeam},
		{"allow rule", AgentMcp{Access: "all", Blocks: []McpAgentBlock{{ServerID: "mcp_a", Name: "x", Effect: "allow"}}}, "only block"},
		{"no server", AgentMcp{Access: "all", Blocks: []McpAgentBlock{{Name: "x", Effect: "block"}}}, "serverId"},
		{"bad rule", AgentMcp{Access: "all", Blocks: []McpAgentBlock{{ServerID: "mcp_a", Effect: "block"}}}, "exactly one"},
		{"many", AgentMcp{Access: "all", Blocks: make([]McpAgentBlock, 501)}, "at most 500"},
	}
	for _, c := range cases {
		got := mcpReasons(c.a.Validate(team))
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
	if len(McpEnv(McpAgentModeFull)) != 3 || len(McpEnv(McpAgentModeSearch)) != 2 {
		t.Error("McpEnv")
	}
}

func TestFederatedIdentity(t *testing.T) {
	allowed := []string{"api://azure-prod", "https://*.okta.example/oauth2/*"}
	f := FederatedIdentity{Name: "azure-prod", Audience: "api://azure-prod"}.WithDefaults()
	if f.FilePath != "/var/run/secrets/afs/federation/azure-prod/token" || f.EnvVar != "AFS_FEDERATED_TOKEN_FILE_AZURE_PROD" || f.ExpirationSeconds != 3600 {
		t.Errorf("defaults = %+v", f)
	}
	cases := []struct {
		name     string
		f        FederatedIdentity
		existing int
		want     string
	}{
		{"ok", f, 0, ""},
		{"glob", FederatedIdentity{Name: "okta", Audience: "https://acme.okta.example/oauth2/default"}, 9, ""},
		{"name", FederatedIdentity{Name: "Azure", Audience: "api://azure-prod"}, 0, "name="},
		{"no audience", FederatedIdentity{Name: "x"}, 0, "audience=required"},
		{"sts", FederatedIdentity{Name: "x", Audience: "sts.amazonaws.com"}, 0, ReasonAudienceReserved},
		{"gateway", FederatedIdentity{Name: "x", Audience: "AFS-GATEWAY"}, 0, ReasonAudienceReserved},
		{"not allowed", FederatedIdentity{Name: "x", Audience: "api://other"}, 0, ReasonAudienceNotAllowed},
		{"short", FederatedIdentity{Name: "x", Audience: "api://azure-prod", ExpirationSeconds: 60}, 0, "expirationSeconds"},
		{"file", FederatedIdentity{Name: "x", Audience: "api://azure-prod", FilePath: "/etc/passwd"}, 0, "filePath"},
		{"env", FederatedIdentity{Name: "x", Audience: "api://azure-prod", EnvVar: "lower"}, 0, "envVar"},
		{"limit", f, 10, ReasonFederatedIdentityLimit},
	}
	for _, c := range cases {
		got := mcpReasons(c.f.Validate(allowed, c.existing))
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMcpWindowsAndReasons(t *testing.T) {
	if ValidateCaptureWindow(7, 7, "incident 42") != nil {
		t.Error("valid window refused")
	}
	if got := mcpReasons(ValidateCaptureWindow(8, 7, "x")); !strings.Contains(got, ReasonCaptureWindowTooLong) {
		t.Errorf("long window = %q", got)
	}
	if got := mcpReasons(ValidateCaptureWindow(1, 7, " ")); !strings.Contains(got, "reason=") {
		t.Errorf("no reason = %q", got)
	}
	if ValidateMcpReason("reason", "ok") != nil || ValidateMcpReason("reason", strings.Repeat("x", 501)) == nil || ValidateMcpReason("note", "") == nil {
		t.Error("ValidateMcpReason")
	}
}

func TestMcpEvents(t *testing.T) {
	checks := map[EventType]EventType{
		McpServerLifecycleEvent("create"):       EventMcpServerCreated,
		McpServerLifecycleEvent("update"):       EventMcpServerUpdated,
		McpServerLifecycleEvent("delete"):       EventMcpServerDeleted,
		McpServerLifecycleEvent("publish"):      EventMcpServerPublished,
		McpServerLifecycleEvent("retire"):       EventMcpServerRetired,
		McpReviewEvent(McpPendingReview, false): EventMcpServerSubmitted,
		McpReviewEvent(McpApproved, false):      EventMcpServerApproved,
		McpReviewEvent(McpRejected, false):      EventMcpServerRejected,
		McpReviewEvent(McpDraft, true):          EventMcpServerChangesRequested,
		McpSuspendEvent(true):                   EventMcpServerSuspended,
		McpSuspendEvent(false):                  EventMcpServerResumed,
		McpPrimitiveEvent("approved"):           EventMcpServerPrimitiveApproved,
		McpPrimitiveEvent("blocked"):            EventMcpServerPrimitiveRejected,
		McpPrimitiveEvent("pending_review"):     EventMcpServerPrimitiveChanged,
		McpCredentialEvent(true):                EventMcpServerCredentialSet,
		McpCredentialEvent(false):               EventMcpServerCredentialRemoved,
		McpAuthEvent(true):                      EventMcpServerAuthExpired,
		McpAuthEvent(false):                     EventMcpServerAuthRestored,
		McpHealthEvent(false):                   EventMcpServerDegraded,
		McpHealthEvent(true):                    EventMcpServerRecovered,
		TeamMcpSwitchEvent(true):                EventTeamMcpServerEnabled,
		TeamMcpSwitchEvent(false):               EventTeamMcpServerDisabled,
		McpCaptureEvent(true):                   EventMcpQueryCaptureStarted,
		McpCaptureEvent(false):                  EventMcpQueryCaptureStopped,
		McpPolicyEvent:                          EventMcpServerPolicyChanged,
		McpSettingsEvent:                        EventMcpSettingsChanged,
		FederatedIdentityEvent:                  EventAgentFederatedIdentityChanged,
	}
	for got, want := range checks {
		if got != want || !got.Known() {
			t.Errorf("%s, want %s", got, want)
		}
	}
	// The contract's §4.14 event types, by their wire names.
	for _, s := range []string{
		"mcp_server.created", "mcp_server.updated", "mcp_server.deleted", "mcp_server.published", "mcp_server.retired",
		"mcp_server.submitted", "mcp_server.approved", "mcp_server.rejected", "mcp_server.changes_requested",
		"mcp_server.suspended", "mcp_server.resumed", "mcp_server.primitive_changed", "mcp_server.primitive_approved",
		"mcp_server.primitive_rejected", "mcp_server.credential_set", "mcp_server.credential_removed",
		"mcp_server.policy_changed", "mcp_server.auth_expired", "mcp_server.auth_restored", "mcp_server.degraded",
		"mcp_server.recovered", "team_mcp_server.enabled", "team_mcp_server.disabled", "mcp_settings.changed",
		"mcp_query_capture.started", "mcp_query_capture.stopped", "agent.federated_identity_changed",
	} {
		if !EventType(s).Known() {
			t.Errorf("%s is not known", s)
		}
	}
}

func TestMcpSettings(t *testing.T) {
	if len(McpSettingSpecs) != 14 {
		t.Fatalf("mcp settings = %d", len(McpSettingSpecs))
	}
	for _, s := range McpSettingSpecs {
		if got, ok := SettingSpecFor(s.Key); !ok || got.Key != s.Key {
			t.Errorf("%s not in the registry", s.Key)
		}
		if err := s.Validate([]byte(s.Default)); err != nil {
			t.Errorf("%s default %s: %v", s.Key, s.Default, err)
		}
		if !IsMcpSetting(s.Key) {
			t.Errorf("%s is not an MCP setting", s.Key)
		}
	}
	if IsMcpSetting(SettingSecretsPathPrefix) {
		t.Error("secrets.path_prefix is an MCP setting")
	}
	spec, _ := SettingSpecFor(SettingMcpTeamServerApproval)
	if spec.Validate([]byte(`"anyone"`)) == nil {
		t.Error("bad approval policy accepted")
	}
	spec, _ = SettingSpecFor(SettingMcpSearchLimitMax)
	if spec.Validate([]byte(`51`)) == nil {
		t.Error("search limit 51 accepted")
	}
	spec, _ = SettingSpecFor(SettingMcpHostDenyList)
	if spec.Validate([]byte(`["*.evil.example","bad host"]`)) == nil {
		t.Error("bad glob accepted")
	}
}
