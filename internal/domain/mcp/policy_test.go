package mcp_test

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

func open() mcp.Access {
	return mcp.Access{Published: true, EnabledForTeam: true, EnabledForAgent: true, TrustAnnotations: true}
}

func tool(name string, a mcp.Annotations) mcp.Primitive {
	return mcp.Primitive{Server: "github", Kind: mcp.KindTool, Name: name, Annotations: a, Status: mcp.StatusApproved}
}

var (
	readOnly = mcp.Annotations{ReadOnlyHint: mcp.Hint(true), OpenWorldHint: mcp.Hint(false)}
	writes   = mcp.Annotations{DestructiveHint: mcp.Hint(false), OpenWorldHint: mcp.Hint(false)}
	destroys = mcp.Annotations{DestructiveHint: mcp.Hint(true), OpenWorldHint: mcp.Hint(false)}
)

func TestEvaluateOrder(t *testing.T) {
	p := tool("get_issue", readOnly)
	block := mcp.LevelPolicy{Rules: []mcp.Rule{{Name: "get_issue", Effect: mcp.EffectBlock}}}
	tests := []struct {
		name string
		mod  func(*mcp.Access)
		p    mcp.Primitive
		want mcp.Reason
	}{
		{"allowed", func(*mcp.Access) {}, p, ""},
		{"not published", func(a *mcp.Access) { a.Published = false; a.EnabledForTeam = false }, p, mcp.ReasonServerNotPublished},
		{"not team", func(a *mcp.Access) { a.EnabledForTeam = false; a.EnabledForAgent = false }, p, mcp.ReasonServerNotEnabledForTeam},
		{"not agent", func(a *mcp.Access) { a.EnabledForAgent = false }, p, mcp.ReasonServerNotEnabledForAgent},
		{"service block wins", func(a *mcp.Access) { a.Service = block; a.Team = block; a.Agent = block }, p, mcp.ReasonBlockedService},
		{"team block", func(a *mcp.Access) { a.Team = block; a.Agent = block }, p, mcp.ReasonBlockedTeam},
		{"agent block", func(a *mcp.Access) { a.Agent = block }, p, mcp.ReasonBlockedAgent},
		{"pending", func(*mcp.Access) {}, mcp.Primitive{Server: "github", Kind: mcp.KindTool, Name: "new", Status: mcp.StatusPendingReview}, mcp.ReasonPendingReview},
		{"status empty", func(*mcp.Access) {}, mcp.Primitive{Server: "github", Kind: mcp.KindTool, Name: "new"}, mcp.ReasonPendingReview},
		{"reviewer blocked", func(*mcp.Access) {}, mcp.Primitive{Server: "github", Kind: mcp.KindTool, Name: "x", Status: mcp.StatusBlocked}, mcp.ReasonBlockedService},
		{"allow at team does not undo service block", func(a *mcp.Access) {
			a.Service = block
			a.Team = mcp.LevelPolicy{Rules: []mcp.Rule{{Name: "get_issue", Effect: mcp.EffectAllow}}}
		}, p, mcp.ReasonBlockedService},
	}
	for _, tt := range tests {
		a := open()
		tt.mod(&a)
		d := a.Evaluate(tt.p)
		if d.Reason != tt.want || d.Allowed != (tt.want == "") {
			t.Errorf("%s: got %+v want %q", tt.name, d, tt.want)
		}
	}
}

func TestRulesWithinLevel(t *testing.T) {
	tests := []struct {
		name    string
		rules   []mcp.Rule
		p       mcp.Primitive
		trusted bool
		blocked bool
	}{
		{"glob block", []mcp.Rule{{Name: "delete_*", Effect: mcp.EffectBlock}}, tool("delete_repo", writes), true, true},
		{"glob miss", []mcp.Rule{{Name: "delete_*", Effect: mcp.EffectBlock}}, tool("get_repo", writes), true, false},
		{"exact allow beats glob block", []mcp.Rule{{Name: "delete_*", Effect: mcp.EffectBlock}, {Name: "delete_branch", Effect: mcp.EffectAllow}}, tool("delete_branch", writes), true, false},
		{"glob allow beats class block", []mcp.Rule{{Class: mcp.ClassDestructive, Effect: mcp.EffectBlock}, {Name: "close_*", Effect: mcp.EffectAllow}}, tool("close_issue", destroys), true, false},
		{"equal specificity block wins", []mcp.Rule{{Name: "x", Effect: mcp.EffectAllow}, {Name: "x", Effect: mcp.EffectBlock}}, tool("x", writes), true, true},
		{"class destructive", []mcp.Rule{{Class: mcp.ClassDestructive, Effect: mcp.EffectBlock}}, tool("drop", destroys), true, true},
		{"class untrusted ignored", []mcp.Rule{{Class: mcp.ClassDestructive, Effect: mcp.EffectBlock}}, tool("drop", destroys), false, false},
		{"class open world", []mcp.Rule{{Class: mcp.ClassOpenWorld, Effect: mcp.EffectBlock}}, tool("fetch", mcp.Annotations{}), true, true},
		{"class read only allow is not a block", []mcp.Rule{{Class: mcp.ClassReadOnly, Effect: mcp.EffectAllow}}, tool("get", readOnly), true, false},
		{"class not for resources", []mcp.Rule{{Class: mcp.ClassOpenWorld, Effect: mcp.EffectBlock}}, mcp.Primitive{Server: "github", Kind: mcp.KindResource, Name: "repo://a", Status: mcp.StatusApproved}, true, false},
		{"uri glob", []mcp.Rule{{Name: "repo://acme/*", Effect: mcp.EffectBlock}}, mcp.Primitive{Server: "github", Kind: mcp.KindResource, Name: "repo://acme/x/y", Status: mcp.StatusApproved}, true, true},
	}
	for _, tt := range tests {
		a := open()
		a.TrustAnnotations = tt.trusted
		a.Team = mcp.LevelPolicy{Rules: tt.rules}
		d := a.Evaluate(tt.p)
		if (d.Reason == mcp.ReasonBlockedTeam) != tt.blocked {
			t.Errorf("%s: got %+v", tt.name, d)
		}
	}
}

func TestReadOnlyMode(t *testing.T) {
	a := open()
	a.Team = mcp.LevelPolicy{ReadOnly: true}
	if d := a.Evaluate(tool("create_issue", writes)); d.Reason != mcp.ReasonBlockedTeam {
		t.Errorf("write tool in read-only mode: %+v", d)
	}
	if d := a.Evaluate(tool("get_issue", readOnly)); !d.Allowed {
		t.Errorf("read tool in read-only mode: %+v", d)
	}
	res := mcp.Primitive{Server: "github", Kind: mcp.KindPrompt, Name: "review", Status: mcp.StatusApproved}
	if d := a.Evaluate(res); !d.Allowed {
		t.Errorf("prompt in read-only mode: %+v", d)
	}
}

func TestRateLimitTightest(t *testing.T) {
	a := open()
	a.Service = mcp.LevelPolicy{Rules: []mcp.Rule{{Name: "search_*", Effect: mcp.EffectAllow, RateLimit: 60}}}
	a.Team = mcp.LevelPolicy{Rules: []mcp.Rule{{Name: "search_code", Effect: mcp.EffectAllow, RateLimit: 10}, {Name: "*", Effect: mcp.EffectAllow, RateLimit: 30}}}
	if d := a.Evaluate(tool("search_code", readOnly)); !d.Allowed || d.RateLimit != 10 {
		t.Errorf("got %+v", d)
	}
	if d := a.Evaluate(tool("get_repo", readOnly)); !d.Allowed || d.RateLimit != 30 {
		t.Errorf("got %+v", d)
	}
}

func TestRuleValid(t *testing.T) {
	tests := []struct {
		r    mcp.Rule
		want bool
	}{
		{mcp.Rule{Name: "x", Effect: mcp.EffectBlock}, true},
		{mcp.Rule{Class: mcp.ClassReadOnly, Effect: mcp.EffectAllow, RateLimit: 5}, true},
		{mcp.Rule{Effect: mcp.EffectBlock}, false},
		{mcp.Rule{Name: "x", Class: mcp.ClassReadOnly, Effect: mcp.EffectBlock}, false},
		{mcp.Rule{Class: "sneaky", Effect: mcp.EffectBlock}, false},
		{mcp.Rule{Name: "x", Effect: "deny"}, false},
		{mcp.Rule{Name: "x", Effect: mcp.EffectAllow, RateLimit: -1}, false},
	}
	for i, tt := range tests {
		if got := tt.r.Valid(); got != tt.want {
			t.Errorf("%d: got %v", i, got)
		}
	}
}

func TestGlob(t *testing.T) {
	tests := []struct {
		p, s string
		want bool
	}{
		{"*", "", true}, {"*", "abc", true}, {"a*", "abc", true}, {"*c", "abc", true}, {"a*c", "abbbc", true},
		{"a*b*c", "a1b2c", true}, {"a*b*c", "a1c", false}, {"a*", "ba", false}, {"*c", "cb", false},
		{"abc", "abc", true}, {"abc", "abd", false}, {"a*a", "a", false},
	}
	for _, tt := range tests {
		if got := mcp.Glob(tt.p, tt.s); got != tt.want {
			t.Errorf("Glob(%q,%q) = %v", tt.p, tt.s, got)
		}
	}
}
