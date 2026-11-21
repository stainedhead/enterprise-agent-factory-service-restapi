package port_test

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func TestSystemSession(t *testing.T) {
	s := port.SystemSession("afs-api")
	if s.Actor != "system:afs-api" || s.ActorType != port.ActorSystem || s.ServiceAdmin || len(s.TeamIDs) != 0 {
		t.Fatalf("got %+v", s)
	}
}

func TestAPIKeyRecordUsable(t *testing.T) {
	tests := []struct {
		name string
		rec  port.APIKeyRecord
		want bool
	}{
		{"active team key", port.APIKeyRecord{TeamID: "team_x", State: "active", TeamState: "active"}, true},
		{"active service key", port.APIKeyRecord{State: "active"}, true},
		{"expired", port.APIKeyRecord{TeamID: "team_x", State: "expired", TeamState: "active"}, false},
		{"revoked", port.APIKeyRecord{State: "revoked"}, false},
		{"suspended team", port.APIKeyRecord{TeamID: "team_x", State: "active", TeamState: "suspended"}, false},
		{"deleting team", port.APIKeyRecord{TeamID: "team_x", State: "active", TeamState: "deleting"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.Usable(); got != tt.want {
				t.Fatalf("Usable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStripSecretKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"id":"key_1","secretKey":"afsk_x"}`, `{"id":"key_1"}`},
		{`[{"a":{"secretKey":"x","b":1}}]`, `[{"a":{"b":1}}]`},
		{`{"secretKeyHint":"ok"}`, `{"secretKeyHint":"ok"}`},
		{``, ``},
	}
	for _, tt := range tests {
		got, err := port.StripSecretKey([]byte(tt.in))
		if err != nil || string(got) != tt.want {
			t.Errorf("StripSecretKey(%s) = %s, %v; want %s", tt.in, got, err, tt.want)
		}
	}
	if _, err := port.StripSecretKey([]byte(`{`)); err == nil {
		t.Error("want error for invalid JSON")
	}
}
