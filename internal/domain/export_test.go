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

func TestRewriteAccounts(t *testing.T) {
	src := TeamBundle{
		Team:    Row{"slug": "p", "allowed_aws_accounts": []any{"111122223333", "payments-*", "*"}},
		Secrets: []Row{{"secrets_manager_arn": "arn:aws:secretsmanager:eu-west-1:111122223333:secret:afs/p/_shared/x-AbCdEf"}}, //nolint:gosec // an ARN in a test fixture, not a credential
		Agents: []AgentBundle{{Agent: Row{"name": "a"},
			Identities: []Row{{"kind": "aws_iam_role", "external_ref": "arn:aws:iam::111122223333:role/afs-a"}, {"kind": "k8s_service_account", "external_ref": "sa-a"}},
			RoleLinks:  []Row{{"account_id": "999988887777", "role_arn": "arn:aws:iam::999988887777:role/deploy"}}}},
	}
	m := map[string]string{"111122223333": "444455556666", "999988887777": "000011112222"}
	got, err := RewriteAccounts(src, m)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Team["allowed_aws_accounts"].([]any); a[0] != "444455556666" || a[1] != "payments-*" || a[2] != "*" {
		t.Errorf("filter %v", a)
	}
	if got.Secrets[0]["secrets_manager_arn"] != "arn:aws:secretsmanager:eu-west-1:444455556666:secret:afs/p/_shared/x-AbCdEf" {
		t.Errorf("secret %v", got.Secrets[0])
	}
	ag := got.Agents[0]
	if ag.Identities[0]["external_ref"] != "arn:aws:iam::444455556666:role/afs-a" || ag.Identities[1]["external_ref"] != "sa-a" ||
		ag.RoleLinks[0]["account_id"] != "000011112222" || ag.RoleLinks[0]["role_arn"] != "arn:aws:iam::000011112222:role/deploy" {
		t.Errorf("agent %+v", ag)
	}
	if src.Secrets[0]["secrets_manager_arn"] == got.Secrets[0]["secrets_manager_arn"] {
		t.Error("the source bundle was changed")
	}
	delete(m, "999988887777")
	if _, err := RewriteAccounts(src, m); err == nil || !strings.Contains(err.Error(), "999988887777") {
		t.Errorf("missing account: %v", err)
	}
}

func TestValidateAccountMap(t *testing.T) {
	if err := ValidateAccountMap(map[string]string{"111122223333": "444455556666"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]string{{"1": "444455556666"}, {"111122223333": "x"}} {
		if KindOf(ValidateAccountMap(m)) != KindValidation {
			t.Errorf("%v accepted", m)
		}
	}
}
