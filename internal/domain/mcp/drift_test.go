package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

func TestCompare(t *testing.T) {
	base := `{"name":"t","description":"Old.","inputSchema":{"type":"object","properties":{"a":{"type":"string","description":"A"}},"required":["a"]},"annotations":{"readOnlyHint":true}}`
	tests := []struct {
		name, next string
		want       mcp.Change
	}{
		{"same", base, mcp.ChangeNone},
		{"description", `{"name":"t","description":"New.","inputSchema":{"type":"object","properties":{"a":{"type":"string","description":"A"}},"required":["a"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeCompatible},
		{"nested description", `{"name":"t","description":"Old.","inputSchema":{"type":"object","properties":{"a":{"type":"string","description":"B"}},"required":["a"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeCompatible},
		{"title added", `{"name":"t","title":"T","description":"Old.","inputSchema":{"type":"object","properties":{"a":{"type":"string","description":"A"}},"required":["a"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeCompatible},
		{"optional added", `{"name":"t","description":"Old.","inputSchema":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeCompatible},
		{"required added", `{"name":"t","description":"Old.","inputSchema":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a","b"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeIncompatible},
		{"property removed", `{"name":"t","description":"Old.","inputSchema":{"type":"object","properties":{},"required":["a"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeIncompatible},
		{"type changed", `{"name":"t","description":"Old.","inputSchema":{"type":"object","properties":{"a":{"type":"integer"}},"required":["a"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeIncompatible},
		{"annotations changed", `{"name":"t","description":"Old.","inputSchema":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},"annotations":{"readOnlyHint":false}}`, mcp.ChangeIncompatible},
		{"key removed", `{"name":"t","description":"Old.","inputSchema":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}`, mcp.ChangeIncompatible},
		{"key added", `{"name":"t","description":"Old.","outputSchema":{},"inputSchema":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeIncompatible},
		{"schema extra changed", `{"name":"t","description":"Old.","inputSchema":{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}},"required":["a"]},"annotations":{"readOnlyHint":true}}`, mcp.ChangeIncompatible},
		{"bad json", `{`, mcp.ChangeIncompatible},
	}
	for _, tt := range tests {
		if got := mcp.Compare(json.RawMessage(base), json.RawMessage(tt.next), "", "x"); got != tt.want {
			t.Errorf("%s: got %s want %s", tt.name, got, tt.want)
		}
	}
	if mcp.Compare(nil, json.RawMessage(base), "", "x") != mcp.ChangeNew {
		t.Error("new")
	}
	if mcp.Compare(json.RawMessage(base), json.RawMessage(`{}`), "d", "d") != mcp.ChangeNone {
		t.Error("equal digests")
	}
	if mcp.Compare(json.RawMessage(`{"inputSchema":true}`), json.RawMessage(`{"inputSchema":false}`), "", "x") != mcp.ChangeIncompatible {
		t.Error("boolean schemas")
	}
}

func TestComparePromptArguments(t *testing.T) {
	base := `{"name":"p","arguments":[{"name":"a","required":true},{"name":"b"}]}`
	tests := []struct {
		next string
		want mcp.Change
	}{
		{`{"name":"p","arguments":[{"name":"a","required":true},{"name":"b"},{"name":"c"}]}`, mcp.ChangeCompatible},
		{`{"name":"p","arguments":[{"name":"a","required":true},{"name":"b"},{"name":"c","required":true}]}`, mcp.ChangeIncompatible},
		{`{"name":"p","arguments":[{"name":"a","required":true}]}`, mcp.ChangeIncompatible},
		{`{"name":"p","arguments":[{"name":"a"},{"name":"b"}]}`, mcp.ChangeIncompatible},
		{`{"name":"p","arguments":"x"}`, mcp.ChangeIncompatible},
	}
	for _, tt := range tests {
		if got := mcp.Compare(json.RawMessage(base), json.RawMessage(tt.next), "", "x"); got != tt.want {
			t.Errorf("%s: got %s", tt.next, got)
		}
	}
}

func TestCrossesRiskLine(t *testing.T) {
	safe := mcp.Annotations{ReadOnlyHint: mcp.Hint(true), OpenWorldHint: mcp.Hint(false)}
	destr := mcp.Annotations{DestructiveHint: mcp.Hint(true), OpenWorldHint: mcp.Hint(false)}
	open := mcp.Annotations{ReadOnlyHint: mcp.Hint(true)}
	tests := []struct {
		before *mcp.Annotations
		after  mcp.Annotations
		kind   mcp.Kind
		want   bool
	}{
		{nil, safe, mcp.KindTool, false},
		{nil, destr, mcp.KindTool, true},
		{&safe, destr, mcp.KindTool, true},
		{&safe, open, mcp.KindTool, true},
		{&destr, destr, mcp.KindTool, false},
		{&safe, safe, mcp.KindTool, false},
		{nil, destr, mcp.KindPrompt, false},
	}
	for i, tt := range tests {
		if got := mcp.CrossesRiskLine(tt.before, tt.after, tt.kind); got != tt.want {
			t.Errorf("%d: got %v", i, got)
		}
	}
}
