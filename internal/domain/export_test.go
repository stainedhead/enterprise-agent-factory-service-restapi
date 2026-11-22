package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleBundle() Bundle {
	return Bundle{SchemaVersion: BundleSchemaVersion, ExportedAt: "2026-11-22T00:00:00Z", ExternalSystems: []Row{},
		Teams: []TeamBundle{{Team: Row{"slug": "payments", "max_budget_usd": json.Number("1000.00")}, Models: []string{"m1"},
			Tags: []Row{{"key": "env", "value": "prod"}}, Agents: []AgentBundle{}}}}
}

func TestChecksumRoundTrip(t *testing.T) {
	sum, doc, err := sampleBundle().Seal()
	if err != nil || len(sum) != 64 {
		t.Fatalf("seal: %q %v", sum, err)
	}
	b, err := ParseBundle(doc)
	if err != nil || b.SHA256 != sum || b.Teams[0].Slug() != "payments" {
		t.Fatalf("parse: %+v %v", b, err)
	}
	sum2, _, _ := b.Checksum()
	if sum2 != sum {
		t.Fatal("checksum is not stable across a round trip")
	}
}

func TestParseBundleRefusals(t *testing.T) {
	_, good, _ := sampleBundle().Seal()
	tamper := strings.Replace(string(good), "payments", "payments2", 1)
	forbidden := func(key string) string {
		b := sampleBundle()
		b.Teams[0].Tags = []Row{{"key": "a", key: "x"}}
		s, doc, _ := b.Checksum() // checksummed, so only the key check can refuse it
		_ = s
		return string(doc)
	}
	newer := sampleBundle()
	newer.SchemaVersion = BundleSchemaVersion + 1
	_, newerDoc, _ := newer.Checksum()
	dup := sampleBundle()
	dup.Teams = append(dup.Teams, dup.Teams[0])
	_, dupDoc, _ := dup.Checksum()
	cases := map[string]string{
		"not json":        "[1",
		"no checksum":     `{"schemaVersion":14}`,
		"mismatch":        tamper,
		"client secret":   forbidden("clientSecret"),
		"key hash":        forbidden("key_sha256"),
		"newer schema":    string(newerDoc),
		"duplicate teams": string(dupDoc),
		"unknown member":  `{"sha256":"` + strings.Repeat("a", 64) + `"}`,
	}
	for name, doc := range cases {
		if _, err := ParseBundle([]byte(doc)); KindOf(err) != KindValidation {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSealRefusesSecrets(t *testing.T) {
	b := sampleBundle()
	b.Teams[0].Secrets = []Row{{"name": "x", "password": "p"}}
	if _, _, err := b.Seal(); err == nil {
		t.Fatal("a bundle with a password member was sealed")
	}
}

func TestCanonicalJSON(t *testing.T) {
	got, err := CanonicalizeJSON([]byte(`{"b": 1.50, "a": ["<x>", null, true], "c": {"z":1,"y":2}}`))
	if err != nil || string(got) != `{"a":["<x>",null,true],"b":1.50,"c":{"y":2,"z":1}}` {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := CanonicalizeJSON([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestValidateExportTeams(t *testing.T) {
	a, b := "team_01JD2Q3R4S5T6V7W8X9Y0Z0001", "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	got, err := ValidateExportTeams([]string{b, a, b})
	if err != nil || len(got) != 2 || got[0] != a {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ValidateExportTeams([]string{"nope"}); KindOf(err) != KindValidation {
		t.Fatal(err)
	}
}

func TestExportExpiry(t *testing.T) {
	now := time.Now()
	exp := now.Add(time.Hour)
	e := Export{DownloadExpiresAt: &exp}
	if e.ExpiredAt(now) || !e.ExpiredAt(exp) {
		t.Fatal("expiry boundary")
	}
}
