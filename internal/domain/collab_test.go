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
	if (Agent{}).IsCollaboration() || !(Agent{Role: AgentRoleCollaboration}).IsCollaboration() {
		t.Error("IsCollaboration")
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

func TestComputeEffectiveAndHostname(t *testing.T) {
	c := TeamCollaboration{WebUIEnabled: true, RESTAPIEnabled: true, MayExpose: true, ProviderClientID: "c", TeamSlug: "acme"}
	s := CollabSettings{BaseDomain: "collab.example.com", PublicEndpoints: true}
	if e := c.ComputeEffective(s); !e.WebUI || !e.RESTAPI || len(e.Reasons) != 0 {
		t.Errorf("%+v", e)
	}
	if c.Hostname(s) != "acme.collab.example.com" || c.Hostname(CollabSettings{}) != "" {
		t.Error("hostname")
	}
	c.ProviderClientID = ""
	if e := c.ComputeEffective(s); e.WebUI || !e.RESTAPI || len(e.Reasons) != 1 || e.Reasons[0] != ReasonProviderWithoutColab {
		t.Errorf("%+v", e)
	}
	c.MayExpose = false
	if e := c.ComputeEffective(CollabSettings{}); e.WebUI || e.RESTAPI || len(e.Reasons) != 4 {
		t.Errorf("%+v", e)
	}
}

func TestValidatePut(t *testing.T) {
	c := TeamCollaboration{MayExpose: true, ProviderClientID: "c"}
	on := CollabSettings{BaseDomain: "x.example.com", PublicEndpoints: true}
	tests := []struct {
		name  string
		c     TeamCollaboration
		p     PutCollaborationParams
		s     CollabSettings
		ready bool
		want  string
	}{
		{"off is always accepted", TeamCollaboration{}, PutCollaborationParams{}, CollabSettings{}, false, ""},
		{"on ok", c, PutCollaborationParams{WebUIEnabled: true, RESTAPIEnabled: true}, on, false, ""},
		{"no base domain", c, PutCollaborationParams{WebUIEnabled: true}, CollabSettings{PublicEndpoints: true}, false, "collab_base_domain_unset"},
		{"endpoints off", c, PutCollaborationParams{RESTAPIEnabled: true}, CollabSettings{BaseDomain: "x"}, false, "collab_public_endpoints_off"},
		{"team not allowed", TeamCollaboration{ProviderClientID: "c"}, PutCollaborationParams{WebUIEnabled: true}, on, false, "collaboration_not_allowed_for_team"},
		{"no client", TeamCollaboration{MayExpose: true}, PutCollaborationParams{WebUIEnabled: true}, on, false, "provider_without_collab_client"},
		{"rest needs no client", TeamCollaboration{MayExpose: true}, PutCollaborationParams{RESTAPIEnabled: true}, on, false, ""},
		{"bad mode", c, PutCollaborationParams{Mode: "x"}, on, true, "must be manual or managed"},
		{"managed not ready", c, PutCollaborationParams{Mode: RegistrationManaged}, on, false, "collaboration_agent_not_ready"},
		{"managed ready", c, PutCollaborationParams{Mode: RegistrationManaged}, on, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.ValidatePut(tc.p, tc.s, tc.ready)
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
