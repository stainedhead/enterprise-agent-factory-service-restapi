package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

func TestEvents(t *testing.T) {
	types := domain.EventTypes()
	if len(types) != 54 || len(domain.EventEmitters) != 54 {
		t.Fatalf("event types = %d", len(types))
	}
	for _, et := range types {
		if !et.Known() || len(domain.EventEmitters[et]) == 0 {
			t.Errorf("%s has no emitter", et)
		}
	}
	e, err := domain.NewEvent(domain.EventTeamCreated, "evt_1", now, "usr_1", "team_1", "team", "team_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e.Detail)
	if string(b) != `{"id":"evt_1","occurredAt":"2026-11-21T18:00:00Z","teamId":"team_1","actor":"usr_1","data":{}}` {
		t.Errorf("envelope = %s", b)
	}
	if _, err := domain.NewEvent("team.exploded", "evt_2", now, "usr_1", "", "team", "x", nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("unknown type err = %v", err)
	}
}

func TestSettings(t *testing.T) {
	if len(domain.SettingSpecs) != 12 {
		t.Fatalf("settings = %d", len(domain.SettingSpecs))
	}
	for _, s := range domain.SettingSpecs {
		if err := s.Validate(json.RawMessage(s.Default)); err != nil {
			t.Errorf("%s default %s invalid: %v", s.Key, s.Default, err)
		}
	}
	cases := []struct {
		key, value string
		ok         bool
	}{
		{domain.SettingSecretsPathPrefix, `"afs-prod"`, true},
		{domain.SettingSecretsPathPrefix, `"Bad"`, false},
		{domain.SettingSecretsPathPrefix, `7`, false},
		{domain.SettingSecretsPathPrefix, `null`, false},
		{domain.SettingPricingPriceListSync, `false`, true},
		{domain.SettingPricingPriceListSync, `"true"`, false},
		{domain.SettingAPIIdempotencyKeyTTLSeconds, `60`, true},
		{domain.SettingAPIIdempotencyKeyTTLSeconds, `59`, false},
		{domain.SettingAPIIdempotencyKeyTTLSeconds, `604801`, false},
		{domain.SettingAPIIdempotencyKeyTTLSeconds, `1.5`, false},
		{domain.SettingAPIIdempotencyKeyTTLSeconds, `"60"`, false},
		{domain.SettingWorkOrdersNotificationChannelID, `null`, true},
		{domain.SettingWorkOrdersNotificationChannelID, `"nch_01JD0000000000000000000000"`, true},
		{domain.SettingWorkOrdersNotificationChannelID, `"sec_01JD0000000000000000000000"`, false},
		{domain.SettingAPICORSOrigins, `["https://console.example.com", "https://a.example.com:8443"]`, true},
		{domain.SettingAPICORSOrigins, `["http://console.example.com"]`, false},
		{domain.SettingAPICORSOrigins, `[1]`, false},
		{domain.SettingAPICORSOrigins, `"https://x.example.com"`, false},
		{domain.SettingAPICORSOrigins, `[] []`, false},
		{domain.SettingAPICORSOrigins, `{`, false},
	}
	for _, c := range cases {
		s, ok := domain.SettingSpecFor(c.key)
		if !ok {
			t.Fatalf("%s missing", c.key)
		}
		if err := s.Validate(json.RawMessage(c.value)); (err == nil) != c.ok {
			t.Errorf("%s = %s: err = %v, want ok=%v", c.key, c.value, err, c.ok)
		}
	}
	if _, ok := domain.SettingSpecFor("nope"); ok {
		t.Error("unknown key found")
	}
}
