package domain

import (
	"bytes"
	"testing"
	"time"
)

func TestNewRoleLink(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		p    NewRoleLinkParams
		ok   bool
	}{
		{"valid", NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/deployer", ProfileName: "prod"}, true},
		{"bad arn", NewRoleLinkParams{RoleARN: "arn:aws:s3:::x", ProfileName: "prod"}, false},
		{"bad profile", NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/d", ProfileName: "Prod"}, false},
		{"short session", NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/d", ProfileName: "p", MaxSessionSeconds: 100}, false},
		{"missing", NewRoleLinkParams{}, false},
	} {
		l, err := NewRoleLink("arl_x", "team_x", "agt_x", "afs-x", tc.p, now)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.ok && (l.AccountID != "111122223333" || l.Status != RoleLinkStatusPendingTrust || l.MaxSessionSeconds != 3600) {
			t.Fatalf("%s: %+v", tc.name, l)
		}
	}
}

func TestExternalIDAndTrustPolicy(t *testing.T) {
	id, err := NewExternalID(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil || len(id) != 36 || id[:4] != "afs-" {
		t.Fatalf("%q %v", id, err)
	}
	if _, err := NewExternalID(bytes.NewReader(nil)); err == nil {
		t.Fatal("short entropy")
	}
	l := RoleLink{ExternalID: id, TeamID: "team_x"}
	if l.TrustPolicy("") != nil || l.TrustPolicy("arn:aws:iam::111122223333:role/afs/a") == nil {
		t.Fatal("trust policy")
	}
}

func TestIdentityParams(t *testing.T) {
	for _, tc := range []struct {
		p  NewAgentIdentityParams
		ok bool
	}{
		{NewAgentIdentityParams{Kind: IdentityGitHubApp, ExternalRef: "12345"}, true},
		{NewAgentIdentityParams{Kind: IdentityAWSIAMRole, ExternalRef: "x"}, false},
		{NewAgentIdentityParams{Kind: IdentityExternal}, false},
		{NewAgentIdentityParams{Kind: IdentityExternal, ExternalRef: "x", SecretID: "nope"}, false},
	} {
		if _, err := NewAgentIdentity("aid_x", "t", "a", tc.p, time.Now()); (err == nil) != tc.ok {
			t.Fatalf("%+v: %v", tc.p, err)
		}
	}
}

func TestGlobAndAccountAllowed(t *testing.T) {
	a := OrgAccount{ID: "111122223333", Name: "payments-prod", Status: OrgAccountActive}
	for _, tc := range []struct {
		pats []string
		acc  OrgAccount
		want bool
	}{
		{[]string{"*"}, a, true},
		{[]string{"111122223333"}, a, true},
		{[]string{"payments-*"}, a, true},
		{[]string{"pay?ents-prod"}, a, true},
		{[]string{"other-*"}, a, false},
		{nil, a, false},
		{[]string{"*"}, OrgAccount{ID: a.ID, Name: a.Name, Status: OrgAccountSuspended}, false},
	} {
		if got := AccountAllowed(tc.pats, tc.acc); got != tc.want {
			t.Fatalf("%v %+v: %v", tc.pats, tc.acc, got)
		}
	}
}
