package mcp_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

func TestKindAndCredentialValid(t *testing.T) {
	for _, k := range []mcp.Kind{mcp.KindTool, mcp.KindResource, mcp.KindResourceTemplate, mcp.KindPrompt} {
		if !k.Valid() {
			t.Errorf("%s should be valid", k)
		}
	}
	if mcp.Kind("sampling").Valid() {
		t.Error("unknown kind valid")
	}
	stores := map[mcp.CredentialKind]bool{
		mcp.CredNone: false, mcp.CredStatic: true, mcp.CredOAuthClient: true,
		mcp.CredWorkloadFederation: false, mcp.CredOAuthUser: true, mcp.CredAWSSigV4: false,
	}
	for k, want := range stores {
		if !k.Valid() || k.StoresSecret() != want {
			t.Errorf("%s: valid %v stores %v", k, k.Valid(), k.StoresSecret())
		}
	}
	if mcp.CredentialKind("password").Valid() {
		t.Error("unknown credential kind valid")
	}
}

func TestQualifiedNames(t *testing.T) {
	p := mcp.Primitive{Server: "github", Name: "create_pull_request"}
	if got := p.QualifiedName(); got != "github__create_pull_request" {
		t.Fatalf("got %q", got)
	}
	tests := []struct {
		in, server, name string
		ok               bool
	}{
		{"github__create_pull_request", "github", "create_pull_request", true},
		{"jira__search__all", "jira", "search__all", true},
		{"__x", "", "", false},
		{"github__", "", "", false},
		{"plain", "", "", false},
	}
	for _, tt := range tests {
		s, n, ok := mcp.SplitQualifiedName(tt.in)
		if s != tt.server || n != tt.name || ok != tt.ok {
			t.Errorf("%q: got %q %q %v", tt.in, s, n, ok)
		}
	}
}

func TestValidateSlug(t *testing.T) {
	tests := []struct {
		in   string
		want error
	}{
		{"github", nil}, {"jira-cloud", nil}, {"s3", nil},
		{"afs", mcp.ErrSlugReserved},
		{"a", mcp.ErrSlugInvalid}, {"GitHub", mcp.ErrSlugInvalid}, {"9lives", mcp.ErrSlugInvalid},
		{"two--dash", mcp.ErrSlugInvalid}, {"trail-", mcp.ErrSlugInvalid}, {"under_score", mcp.ErrSlugInvalid},
		{strings.Repeat("a", 32), mcp.ErrSlugInvalid},
	}
	for _, tt := range tests {
		if err := mcp.ValidateSlug(tt.in); !errors.Is(err, tt.want) {
			t.Errorf("%q: got %v want %v", tt.in, err, tt.want)
		}
	}
	if got := mcp.EnvName("jira-cloud"); got != "JIRA_CLOUD" {
		t.Errorf("EnvName = %q", got)
	}
}

func TestAnnotationDefaults(t *testing.T) {
	var none mcp.Annotations
	if none.ReadOnly() || !none.Destructive() || none.Idempotent() || !none.OpenWorld() {
		t.Error("absent hints must take the MCP defaults")
	}
	ro := mcp.Annotations{ReadOnlyHint: mcp.Hint(true), DestructiveHint: mcp.Hint(true)}
	if !ro.ReadOnly() || ro.Destructive() {
		t.Error("a read-only tool is never destructive")
	}
	safe := mcp.Annotations{DestructiveHint: mcp.Hint(false), OpenWorldHint: mcp.Hint(false), IdempotentHint: mcp.Hint(true)}
	if safe.Destructive() || safe.OpenWorld() || !safe.Idempotent() {
		t.Error("explicit false hints must be honoured")
	}
}

func TestApproval(t *testing.T) {
	tests := []struct {
		p    mcp.ApprovalPolicy
		t    mcp.Tier
		want mcp.Approver
	}{
		{mcp.ApprovalTeamAll, mcp.TierLow, mcp.ApproverTeamAdmin},
		{mcp.ApprovalTeamAll, mcp.TierHigh, mcp.ApproverTeamAdmin},
		{mcp.ApprovalTeamAll, "bogus", mcp.ApproverSecurity},
		{mcp.ApprovalTeamLowSecurityHigher, mcp.TierLow, mcp.ApproverTeamAdmin},
		{mcp.ApprovalTeamLowSecurityHigher, mcp.TierElevated, mcp.ApproverSecurity},
		{mcp.ApprovalTeamLowSecurityHigher, mcp.TierHigh, mcp.ApproverSecurity},
		{mcp.ApprovalSecurityAll, mcp.TierLow, mcp.ApproverSecurity},
		{"unknown", mcp.TierLow, mcp.ApproverSecurity},
	}
	for _, tt := range tests {
		if got := mcp.RequiredApprover(tt.p, tt.t); got != tt.want {
			t.Errorf("%s/%s: got %s want %s", tt.p, tt.t, got, tt.want)
		}
	}
	for _, p := range []mcp.ApprovalPolicy{mcp.ApprovalTeamAll, mcp.ApprovalTeamLowSecurityHigher, mcp.ApprovalSecurityAll} {
		if !p.Valid() {
			t.Errorf("%s invalid", p)
		}
	}
	if mcp.ApprovalPolicy("x").Valid() {
		t.Error("unknown policy valid")
	}
	if mcp.Max(mcp.TierLow, mcp.TierElevated) != mcp.TierElevated || mcp.Max(mcp.TierHigh, mcp.TierLow) != mcp.TierHigh ||
		mcp.Max("bogus", mcp.TierLow) != mcp.TierHigh || mcp.Max(mcp.TierLow, mcp.TierLow) != mcp.TierLow {
		t.Error("Max")
	}
}
