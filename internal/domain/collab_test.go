package domain

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCollabEnumsValid(t *testing.T) {
	for _, v := range []AgentRole{AgentRoleWorker, AgentRoleCollaboration} {
		if !v.Valid() {
			t.Errorf("%s", v)
		}
	}
	if AgentRole("x").Valid() || RegistrationMode("x").Valid() || LinkState("x").Valid() || SecretManagedBy("x").Valid() {
		t.Error("unknown value accepted")
	}
	for _, v := range []LinkState{LinkPending, LinkLinked, LinkFailed, LinkDisabled} {
		if !v.Valid() {
			t.Errorf("%s", v)
		}
	}
	for _, v := range []SecretManagedBy{ManagedByUser, ManagedByAFS, ManagedByPaperclip} {
		if !v.Valid() {
			t.Errorf("%s", v)
		}
	}
	if !RegistrationManual.Valid() || !RegistrationManaged.Valid() {
		t.Error("registration modes")
	}
	if (Agent{}).IsOrchestration() || !(Agent{Role: AgentRoleOrchestration}).IsOrchestration() || !(Agent{Role: AgentRoleCollaboration}).IsOrchestration() {
		t.Error("IsOrchestration")
	}
	if AgentRoleCollaboration.Normalize() != AgentRoleOrchestration || AgentRoleWorker.Normalize() != AgentRoleWorker {
		t.Error("Normalize")
	}
	if (PutBudgetParams{}).RequestsNotify() || !(PutBudgetParams{OnExhaust: OnExhaustNotify}).RequestsNotify() {
		t.Error("RequestsNotify")
	}
	if ValidateCapabilityFilter("") != nil || ValidateCapabilityFilter(CapabilityPaperclip) != nil || ValidateCapabilityFilter("x") == nil {
		t.Error("ValidateCapabilityFilter")
	}
}

func TestBootSecretsAndNames(t *testing.T) {
	if len(BootSecrets()) != 3 || len(RotatableCollabKeys()) != 2 {
		t.Fatal("boot secrets")
	}
	for _, n := range RotatableCollabKeys() {
		if n == SecretPaperclipMasterKey {
			t.Error("master key is rotatable")
		}
	}
	for name, want := range map[string]bool{"paperclip": true, "paperclip-x": true, "paperclips": false, "other": false} {
		if IsReservedSecretName(name) != want {
			t.Errorf("%s", name)
		}
	}
	if ReservedSecretName("paperclip").Error() == "" || ManagedSecret(ManagedByAFS).Reason != ReasonManagedSecret ||
		CollaborationAgentExists().Reason != ReasonCollaborationAgentExists {
		t.Error("errors")
	}
}

func TestComputeSurface(t *testing.T) {
	all := OrchestrationSettings{PublicBaseDomain: "collab.example.com", PrivateBaseDomain: "collab.corp.example.com", PublicEndpoints: true, PrivateEndpoints: true}
	tests := []struct {
		name    string
		tool    CollabTool
		surface Surface
		chosen  Exposure
		limit   Exposure
		client  bool
		s       OrchestrationSettings
		eff     Exposure
		host    string
		reasons []string
	}{
		{"public within limit", ToolPaperclip, SurfaceWebUI, ExposurePublic, ExposurePublic, true, all, ExposurePublic, "acme.collab.example.com", nil},
		{"private host", ToolPaperclip, SurfaceWebAPI, ExposurePrivate, ExposurePublic, false, all, ExposurePrivate, "acme.collab.corp.example.com", nil},
		{"lowered to limit", ToolPaperclip, SurfaceWebUI, ExposurePublic, ExposurePrivate, true, all, ExposurePrivate, "acme.collab.corp.example.com", []string{ReasonTeamLimit}},
		{"buzz has no surface", ToolBuzz, SurfaceWebUI, ExposurePublic, ExposurePublic, true, all, ExposureOff, "", []string{ReasonToolNotSupported}},
		{"public switch off", ToolPaperclip, SurfaceWebAPI, ExposurePublic, ExposurePublic, true, OrchestrationSettings{PublicBaseDomain: "x"}, ExposureOff, "", []string{ReasonServiceSwitchPublic}},
		{"private switch off", ToolPaperclip, SurfaceWebAPI, ExposurePrivate, ExposurePublic, true, OrchestrationSettings{PrivateBaseDomain: "x", PublicEndpoints: true}, ExposureOff, "", []string{ReasonServiceSwitchPrivate}},
		{"no base domain", ToolPaperclip, SurfaceWebAPI, ExposurePublic, ExposurePublic, true, OrchestrationSettings{PublicEndpoints: true}, ExposureOff, "", []string{ReasonBaseDomainUnsetNew}},
		{"no client", ToolPaperclip, SurfaceWebUI, ExposurePublic, ExposurePublic, false, all, ExposureOff, "", []string{ReasonProviderWithoutClient}},
		{"off stays off", ToolPaperclip, SurfaceWebUI, ExposureOff, ExposureOff, false, OrchestrationSettings{}, ExposureOff, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := ComputeSurface(tc.tool, tc.surface, tc.chosen, tc.limit, tc.client, "acme", tc.s)
			if st.Effective != tc.eff || st.Hostname != tc.host || st.Exposure != tc.chosen || strings.Join(st.Reasons, ",") != strings.Join(tc.reasons, ",") {
				t.Errorf("%+v", st)
			}
			wantURL := ""
			if tc.host != "" {
				wantURL = "https://" + tc.host + "/"
				if tc.surface == SurfaceWebAPI {
					wantURL += "api"
				}
			}
			if st.URL(tc.surface) != wantURL {
				t.Errorf("url %q want %q", st.URL(tc.surface), wantURL)
			}
		})
	}
}

func TestSurfacesAndLegacy(t *testing.T) {
	c := TeamCollaboration{Tool: ToolPaperclip, WebUIExposure: ExposurePublic, WebAPIExposure: ExposurePrivate, Limit: ExposurePrivate, ProviderClientID: "c", TeamSlug: "acme"}
	s := OrchestrationSettings{PublicBaseDomain: "collab.example.com", PrivateBaseDomain: "corp.example.com", PublicEndpoints: true, PrivateEndpoints: true}
	ui, api := c.Surfaces(s)
	if ui.Effective != ExposurePrivate || api.Effective != ExposurePrivate {
		t.Fatalf("%+v %+v", ui, api)
	}
	l := LegacyOf(ui, api)
	if !l.WebUI || !l.RESTAPI || len(l.Reasons) != 1 || l.Reasons[0] != ReasonTeamNotAllowed {
		t.Errorf("%+v", l)
	}
	l = LegacyOf(SurfaceState{Effective: ExposureOff, Reasons: []string{ReasonServiceSwitchPublic}}, SurfaceState{Effective: ExposureOff, Reasons: []string{ReasonServiceSwitchPrivate, ReasonBaseDomainUnsetNew}})
	if l.WebUI || len(l.Reasons) != 2 || l.Reasons[0] != ReasonServiceEndpointsOff {
		t.Errorf("%+v", l)
	}
	if c.LegacyHostname(s) != "acme.corp.example.com" {
		t.Error("effective hostname")
	}
	if (TeamCollaboration{Tool: ToolBuzz, TeamSlug: "acme"}).LegacyHostname(s) != "acme.collab.example.com" ||
		(TeamCollaboration{Tool: ToolBuzz, TeamSlug: "acme"}).LegacyHostname(OrchestrationSettings{}) != "" {
		t.Error("fallback hostname")
	}
}

func TestValidatePutOrchestration(t *testing.T) {
	pc := TeamCollaboration{Tool: ToolPaperclip, Limit: ExposurePrivate}
	bz := TeamCollaboration{Tool: ToolBuzz, Limit: ExposurePublic}
	tests := []struct {
		name  string
		c     TeamCollaboration
		p     PutOrchestrationParams
		ready bool
		want  string
	}{
		{"off is accepted", pc, PutOrchestrationParams{WebUI: ExposureOff, WebAPI: ExposureOff}, false, ""},
		{"within limit", pc, PutOrchestrationParams{WebUI: ExposurePrivate, WebAPI: ExposureOff}, false, ""},
		{"above limit", pc, PutOrchestrationParams{WebUI: ExposurePublic, WebAPI: ExposureOff}, false, ReasonExposureAboveLimit},
		{"buzz surface", bz, PutOrchestrationParams{WebUI: ExposurePrivate, WebAPI: ExposureOff}, false, ReasonSurfaceNotSupported},
		{"buzz off ok", bz, PutOrchestrationParams{WebUI: ExposureOff, WebAPI: ExposureOff, Mode: RegistrationManaged}, true, ""},
		{"buzz manual", bz, PutOrchestrationParams{WebUI: ExposureOff, WebAPI: ExposureOff, Mode: RegistrationManual}, true, ReasonRegistrationNotSupported},
		{"bad level", pc, PutOrchestrationParams{WebUI: "x", WebAPI: ExposureOff}, false, "must be off, private or public"},
		{"bad mode", pc, PutOrchestrationParams{WebUI: ExposureOff, WebAPI: ExposureOff, Mode: "x"}, true, "must be manual or managed"},
		{"managed not ready", pc, PutOrchestrationParams{WebUI: ExposureOff, WebAPI: ExposureOff, Mode: RegistrationManaged}, false, "collaboration_agent_not_ready"},
		{"managed ready", pc, PutOrchestrationParams{WebUI: ExposureOff, WebAPI: ExposureOff, Mode: RegistrationManaged}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.ValidatePutOrchestration(tc.p, tc.ready)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var de *Error
			if !errors.As(err, &de) || len(de.Fields) == 0 || de.Fields[0].Reason != tc.want {
				t.Fatalf("got %v want %s", err, tc.want)
			}
		})
	}
}

func TestLegacyPutToOrchestration(t *testing.T) {
	pc := TeamCollaboration{Tool: ToolPaperclip, Limit: ExposurePrivate}
	p, err := pc.LegacyPutToOrchestration(true, false, "")
	if err != nil || p.WebUI != ExposurePrivate || p.WebAPI != ExposureOff {
		t.Errorf("%+v %v", p, err)
	}
	for name, tc := range map[string]struct {
		c      TeamCollaboration
		reason string
	}{
		"limit off": {TeamCollaboration{Tool: ToolPaperclip, Limit: ExposureOff}, ReasonCollabNotAllowedForTeam},
		"buzz":      {TeamCollaboration{Tool: ToolBuzz, Limit: ExposurePublic}, ReasonSurfaceNotSupported},
	} {
		_, err := tc.c.LegacyPutToOrchestration(true, false, "")
		var de *Error
		if !errors.As(err, &de) || de.Fields[0].Reason != tc.reason {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOrchestrationPending(t *testing.T) {
	if OrchestrationPending(false).Kind != KindNotFound || OrchestrationPending(true).Kind != KindConflict ||
		OrchestrationPending(true).Reason != ReasonOrchestrationAgentPending {
		t.Error("pending")
	}
}

func TestPaperclipAgentIDAndInput(t *testing.T) {
	if ValidatePaperclipAgentID("") != nil || ValidatePaperclipAgentID("a.b:c-1") != nil || ValidatePaperclipAgentID("a b") == nil ||
		ValidatePaperclipAgentID(strings.Repeat("a", 129)) == nil {
		t.Error("ValidatePaperclipAgentID")
	}
	yes, no := true, false
	if (CollaborationInput{}).Wants() || !(CollaborationInput{Connect: &yes}).Wants() || (CollaborationInput{Connect: &no}).Wants() {
		t.Error("Wants")
	}
	if (CollaborationInput{Connect: &yes, PaperclipAgentID: "a"}).Validate() != nil ||
		(CollaborationInput{Connect: &no, PaperclipAgentID: "a"}).Validate() == nil ||
		(CollaborationInput{PaperclipAgentID: "a b"}).Validate() == nil {
		t.Error("Validate")
	}
}

func TestCollaborationAgentMissingAndGenerate(t *testing.T) {
	if CollaborationAgentMissing(true).Kind != KindConflict || CollaborationAgentMissing(false).Kind != KindNotFound {
		t.Error("kinds")
	}
	v, err := GenerateSecretValue(bytes.NewReader(bytes.Repeat([]byte{0xab}, 8)), 4)
	if err != nil || v.Expose() != "abababab" {
		t.Errorf("%v", err)
	}
	if _, err := GenerateSecretValue(bytes.NewReader(nil), 4); err == nil {
		t.Error("short source accepted")
	}
}

func TestValidateCollabSetting(t *testing.T) {
	ok := []string{`[]`, `["10.0.0.0/8","2001:db8::/32"]`}
	for _, v := range ok {
		if err := ValidateCollabSetting(SettingCollabAllowedSourceCidrs, []byte(v)); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	for _, v := range []string{`["999.1.1.1/99"]`, `["10.0.0.0"]`, `{}`} {
		if ValidateCollabSetting(SettingCollabAllowedSourceCidrs, []byte(v)) == nil {
			t.Errorf("accepted %s", v)
		}
	}
	if ValidateCollabSetting(SettingCollabBaseDomain, []byte(`"x"`)) != nil {
		t.Error("another key was checked")
	}
}
